package nebula

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/slackhq/nebula/cert"
)

type LightHouse struct {
	sync.RWMutex //Because we concurrently read and write to our maps
	amLighthouse bool
	myIp         uint32
	punchConn    *udpConn

	// Local cache of answers from light houses
	addrMap map[uint32][]udpAddr

	// staticList exists to avoid having a bool in each addrMap entry
	// since static should be rare
	staticList  map[uint32]struct{}
	lighthouses map[uint32]struct{}
	interval    int
	nebulaPort  int
	punchBack   bool
}

type EncWriter interface {
	SendMessageToVpnIp(t NebulaMessageType, st NebulaMessageSubType, vpnIp uint32, p, nb, out []byte)
	SendMessageToAll(t NebulaMessageType, st NebulaMessageSubType, vpnIp uint32, p, nb, out []byte)
}

func NewLightHouse(amLighthouse bool, myIp uint32, ips []uint32, interval int, nebulaPort int, pc *udpConn, punchBack bool) *LightHouse {
	h := LightHouse{
		amLighthouse: amLighthouse,
		myIp:         myIp,
		addrMap:      make(map[uint32][]udpAddr),
		nebulaPort:   nebulaPort,
		lighthouses:  make(map[uint32]struct{}),
		staticList:   make(map[uint32]struct{}),
		interval:     interval,
		punchConn:    pc,
		punchBack:    punchBack,
	}

	for _, ip := range ips {
		h.lighthouses[ip] = struct{}{}
	}

	return &h
}

func NewLightHouseFromConfig(config *Config, myIp uint32, nebulaPort int, pc *udpConn) (*LightHouse, error) {
	h := LightHouse{
		amLighthouse: config.GetBool("lighthouse.am_lighthouse", false),
		myIp:         myIp,
		addrMap:      make(map[uint32][]udpAddr),
		nebulaPort:   nebulaPort,
		lighthouses:  make(map[uint32]struct{}),
		staticList:   make(map[uint32]struct{}),
		interval:     config.GetInt("lighthouse.interval", 10),
		punchConn:    pc,
		punchBack:    config.GetBool("punch_back", false),
	}

	err := h.loadStaticHosts(config)
	if err != nil {
		return nil, err
	}

	config.RegisterReloadCallback(h.reload)

	return &h, nil
}

func (lh *LightHouse) loadStaticHosts(config *Config) error {
	rawLighthouseHosts := config.GetStringSlice("lighthouse.hosts", []string{})
	if lh.amLighthouse && len(rawLighthouseHosts) != 0 {
		l.Warn("lighthouse.am_lighthouse enabled on node but upstream lighthouses exist in config")
	}

	for i, host := range rawLighthouseHosts {
		ip := net.ParseIP(host)
		if ip == nil {
			l.WithField("host", host).Fatalf("Unable to parse lighthouse host entry %v", i+1)
		}
		lh.lighthouses[ip2int(ip)] = struct{}{}
	}

	for k, v := range config.GetMap("static_host_map", map[interface{}]interface{}{}) {
		vpnIp := net.ParseIP(fmt.Sprintf("%v", k))
		vals, ok := v.([]interface{})
		if ok {
			for _, v := range vals {
				err := lh.addStaticRemote(vpnIp, v)
				if err != nil {
					return err
				}
			}
		} else {
			err := lh.addStaticRemote(vpnIp, v)
			if err != nil {
				return err
			}
		}
	}

	err := lh.ValidateLHStaticEntries()
	if err != nil {
		l.WithError(err).Error("Lighthouse unreachable")
	}

	return nil
}

func (lh *LightHouse) reload(config *Config) {
	if config.GetBool("lighthouse.am_lighthouse", false) != lh.amLighthouse {
		l.Warn("Changing lighthouse.am_lighthouse with SIGHUP not supported. Ignored.")
	}
	lh.interval = config.GetInt("lighthouse.interval", 10)
	lh.punchBack = config.GetBool("punch_back", false)

	err := lh.loadStaticHosts(config)
	if err != nil {
		l.WithError(err).Error("failed to reload static hosts")
	}

	// Check for removed lighthouses / static entries. load the new config
	// into a fresh LightHouse and then see which entries are no longer there.

	newLh := LightHouse{
		addrMap:     make(map[uint32][]udpAddr),
		lighthouses: make(map[uint32]struct{}),
		staticList:  make(map[uint32]struct{}),
	}
	newLh.loadStaticHosts(config)

	var removedStatic, removedLighthouses []uint32
	for vpnIP := range lh.staticList {
		if _, ok := newLh.staticList[vpnIP]; !ok {
			removedStatic = append(removedStatic, vpnIP)
		}
	}
	for vpnIP := range lh.lighthouses {
		if _, ok := newLh.lighthouses[vpnIP]; !ok {
			removedLighthouses = append(removedLighthouses, vpnIP)
		}
	}

	for _, vpnIP := range removedLighthouses {
		l.Infof("lighthouse.hosts entry removed: %s", IntIp(vpnIP))
		delete(lh.lighthouses, vpnIP)
	}
	for _, vpnIP := range removedStatic {
		l.Infof("static_host_map entry removed: %s", IntIp(vpnIP))
		delete(lh.staticList, vpnIP)
		lh.DeleteVpnIP(vpnIP)
	}
}

func (lh *LightHouse) ValidateLHStaticEntries() error {
	for lhIP, _ := range lh.lighthouses {
		if _, ok := lh.staticList[lhIP]; !ok {
			return fmt.Errorf("Lighthouse %s does not have a static_host_map entry", IntIp(lhIP))
		}
	}
	return nil
}

func (lh *LightHouse) Query(ip uint32, f EncWriter) ([]udpAddr, error) {
	if !lh.IsLighthouseIP(ip) {
		lh.QueryServer(ip, f)
	}
	lh.RLock()
	if v, ok := lh.addrMap[ip]; ok {
		lh.RUnlock()
		return v, nil
	}
	lh.RUnlock()
	return nil, fmt.Errorf("host %s not known, queries sent to lighthouses", IntIp(ip))
}

// This is asynchronous so no reply should be expected
func (lh *LightHouse) QueryServer(ip uint32, f EncWriter) {
	if !lh.amLighthouse {
		// Send a query to the lighthouses and hope for the best next time
		query, err := proto.Marshal(NewLhQueryByInt(ip))
		if err != nil {
			l.WithError(err).WithField("vpnIp", IntIp(ip)).Error("Failed to marshal lighthouse query payload")
			return
		}

		nb := make([]byte, 12, 12)
		out := make([]byte, mtu)
		for n := range lh.lighthouses {
			f.SendMessageToVpnIp(lightHouse, 0, n, query, nb, out)
		}
	}
}

// Query our local lighthouse cached results
func (lh *LightHouse) QueryCache(ip uint32) []udpAddr {
	lh.RLock()
	if v, ok := lh.addrMap[ip]; ok {
		lh.RUnlock()
		return v
	}
	lh.RUnlock()
	return nil
}

func (lh *LightHouse) DeleteVpnIP(vpnIP uint32) {
	// First we check the static mapping
	// and do nothing if it is there
	if _, ok := lh.staticList[vpnIP]; ok {
		return
	}
	lh.Lock()
	//l.Debugln(lh.addrMap)
	delete(lh.addrMap, vpnIP)
	l.Debugf("deleting %s from lighthouse.", IntIp(vpnIP))
	lh.Unlock()
}

func (lh *LightHouse) addStaticRemote(vpnIp net.IP, v interface{}) error {
	parts := strings.Split(fmt.Sprintf("%v", v), ":")
	addr, err := net.ResolveIPAddr("ip", parts[0])
	if err != nil {
		return err
	}

	ip := addr.IP
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("Static host address for %s could not be parsed: %s", vpnIp, v)
	}

	lh.AddRemote(ip2int(vpnIp), NewUDPAddr(ip2int(ip), uint16(port)), true)
	return nil
}

func (lh *LightHouse) AddRemote(vpnIP uint32, toIp *udpAddr, static bool) {
	// First we check if the sender thinks this is a static entry
	// and do nothing if it is not, but should be considered static
	if static == false {
		if _, ok := lh.staticList[vpnIP]; ok {
			return
		}
	}

	lh.Lock()
	for _, v := range lh.addrMap[vpnIP] {
		if v.Equals(toIp) {
			lh.Unlock()
			return
		}
	}
	//l.Debugf("Adding reply of %s as %s\n", IntIp(vpnIP), toIp)
	if static {
		lh.staticList[vpnIP] = struct{}{}
	}
	lh.addrMap[vpnIP] = append(lh.addrMap[vpnIP], *toIp)
	lh.Unlock()
}

func (lh *LightHouse) AddRemoteAndReset(vpnIP uint32, toIp *udpAddr) {
	if lh.amLighthouse {
		lh.DeleteVpnIP(vpnIP)
		lh.AddRemote(vpnIP, toIp, false)
	}

}

func (lh *LightHouse) IsLighthouseIP(vpnIP uint32) bool {
	if _, ok := lh.lighthouses[vpnIP]; ok {
		return true
	}
	return false
}

// Quick generators for protobuf

func NewLhQueryByIpString(VpnIp string) *NebulaMeta {
	return NewLhQueryByInt(ip2int(net.ParseIP(VpnIp)))
}

func NewLhQueryByInt(VpnIp uint32) *NebulaMeta {
	return &NebulaMeta{
		Type: NebulaMeta_HostQuery,
		Details: &NebulaMetaDetails{
			VpnIp: VpnIp,
		},
	}
}

func NewLhWhoami() *NebulaMeta {
	return &NebulaMeta{
		Type:    NebulaMeta_HostWhoami,
		Details: &NebulaMetaDetails{},
	}
}

// End Quick generators for protobuf

func NewIpAndPortFromUDPAddr(addr udpAddr) *IpAndPort {
	return &IpAndPort{Ip: udp2ipInt(&addr), Port: uint32(addr.Port)}
}

func NewIpAndPortsFromNetIps(ips []udpAddr) *[]*IpAndPort {
	var iap []*IpAndPort
	for _, e := range ips {
		// Only add IPs that aren't my VPN/tun IP
		iap = append(iap, NewIpAndPortFromUDPAddr(e))
	}
	return &iap
}

func (lh *LightHouse) LhUpdateWorker(f EncWriter) {
	if lh.amLighthouse || lh.interval == 0 {
		return
	}

	for {
		ipp := []*IpAndPort{}

		for _, e := range *localIps() {
			// Only add IPs that aren't my VPN/tun IP
			if ip2int(e) != lh.myIp {
				ipp = append(ipp, &IpAndPort{Ip: ip2int(e), Port: uint32(lh.nebulaPort)})
				//fmt.Println(e)
			}
		}
		m := &NebulaMeta{
			Type: NebulaMeta_HostUpdateNotification,
			Details: &NebulaMetaDetails{
				VpnIp:      lh.myIp,
				IpAndPorts: ipp,
			},
		}

		nb := make([]byte, 12, 12)
		out := make([]byte, mtu)
		for vpnIp := range lh.lighthouses {
			mm, err := proto.Marshal(m)
			if err != nil {
				l.Debugf("Invalid marshal to update")
			}
			//l.Error("LIGHTHOUSE PACKET SEND", mm)
			f.SendMessageToVpnIp(lightHouse, 0, vpnIp, mm, nb, out)

		}
		time.Sleep(time.Second * time.Duration(lh.interval))
	}
}

func (lh *LightHouse) HandleRequest(rAddr *udpAddr, vpnIp uint32, p []byte, c *cert.NebulaCertificate, f EncWriter) {
	n := &NebulaMeta{}
	err := proto.Unmarshal(p, n)
	if err != nil {
		l.WithError(err).WithField("vpnIp", IntIp(vpnIp)).WithField("udpAddr", rAddr).
			Error("Failed to unmarshal lighthouse packet")
		//TODO: send recv_error?
		return
	}

	if n.Details == nil {
		l.WithField("vpnIp", IntIp(vpnIp)).WithField("udpAddr", rAddr).
			Error("Invalid lighthouse update")
		//TODO: send recv_error?
		return
	}

	switch n.Type {
	case NebulaMeta_HostQuery:
		// Exit if we don't answer queries
		if !lh.amLighthouse {
			l.Debugln("I don't answer queries, but received from: ", rAddr)
			return
		}

		//l.Debugln("Got Query")
		ips, err := lh.Query(n.Details.VpnIp, f)
		if err != nil {
			//l.Debugf("Can't answer query %s from %s because error: %s", IntIp(n.Details.VpnIp), rAddr, err)
			return
		} else {
			iap := NewIpAndPortsFromNetIps(ips)
			answer := &NebulaMeta{
				Type: NebulaMeta_HostQueryReply,
				Details: &NebulaMetaDetails{
					VpnIp:      n.Details.VpnIp,
					IpAndPorts: *iap,
				},
			}
			reply, err := proto.Marshal(answer)
			if err != nil {
				l.WithError(err).WithField("vpnIp", IntIp(vpnIp)).Error("Failed to marshal lighthouse host query reply")
				return
			}
			f.SendMessageToVpnIp(lightHouse, 0, vpnIp, reply, make([]byte, 12, 12), make([]byte, mtu))

			// This signals the other side to punch some zero byte udp packets
			ips, err = lh.Query(vpnIp, f)
			if err != nil {
				l.WithField("vpnIp", IntIp(vpnIp)).Debugln("Can't notify host to punch")
				return
			} else {
				//l.Debugln("Notify host to punch", iap)
				iap = NewIpAndPortsFromNetIps(ips)
				answer = &NebulaMeta{
					Type: NebulaMeta_HostPunchNotification,
					Details: &NebulaMetaDetails{
						VpnIp:      vpnIp,
						IpAndPorts: *iap,
					},
				}
				reply, _ := proto.Marshal(answer)
				f.SendMessageToVpnIp(lightHouse, 0, n.Details.VpnIp, reply, make([]byte, 12, 12), make([]byte, mtu))
			}
			//fmt.Println(reply, remoteaddr)
		}

	case NebulaMeta_HostQueryReply:
		if !lh.IsLighthouseIP(vpnIp) {
			return
		}
		for _, a := range n.Details.IpAndPorts {
			//first := n.Details.IpAndPorts[0]
			ans := NewUDPAddr(a.Ip, uint16(a.Port))
			lh.AddRemote(n.Details.VpnIp, ans, false)
		}

	case NebulaMeta_HostUpdateNotification:
		//Simple check that the host sent this not someone else
		if n.Details.VpnIp != vpnIp {
			l.WithField("vpnIp", IntIp(vpnIp)).WithField("answer", IntIp(n.Details.VpnIp)).Debugln("Host sent invalid update")
			return
		}
		for _, a := range n.Details.IpAndPorts {
			ans := NewUDPAddr(a.Ip, uint16(a.Port))
			lh.AddRemote(n.Details.VpnIp, ans, false)
		}
	case NebulaMeta_HostMovedNotification:
	case NebulaMeta_HostPunchNotification:
		if !lh.IsLighthouseIP(vpnIp) {
			return
		}

		empty := []byte{0}
		for _, a := range n.Details.IpAndPorts {
			vpnPeer := NewUDPAddr(a.Ip, uint16(a.Port))
			go func() {
				for i := 0; i < 5; i++ {
					lh.punchConn.WriteTo(empty, vpnPeer)
					time.Sleep(time.Second * 1)
				}

			}()
			l.Debugf("Punching %s on %d for %s", IntIp(a.Ip), a.Port, IntIp(n.Details.VpnIp))
		}
		// This sends a nebula test packet to the host trying to contact us. In the case
		// of a double nat or other difficult scenario, this may help establish
		// a tunnel.
		if lh.punchBack {
			go func() {
				time.Sleep(time.Second * 5)
				l.Debugf("Sending a nebula test packet to vpn ip %s", IntIp(n.Details.VpnIp))
				f.SendMessageToVpnIp(test, testRequest, n.Details.VpnIp, []byte(""), make([]byte, 12, 12), make([]byte, mtu))
			}()
		}
	}
}

/*
func (f *Interface) sendPathCheck(ci *ConnectionState, endpoint *net.UDPAddr, counter int) {
	c := ci.messageCounter
    b := HeaderEncode(nil, Version, uint8(path_check), 0, ci.remoteIndex, c)
	ci.messageCounter++

	if ci.eKey != nil {
		msg := ci.eKey.EncryptDanger(b, nil, []byte(strconv.Itoa(counter)), c)
		//msg := ci.eKey.EncryptDanger(b, nil, []byte(fmt.Sprintf("%d", counter)), c)
		f.outside.WriteTo(msg, endpoint)
		l.Debugf("path_check sent, remote index: %d, pathCounter %d", ci.remoteIndex, counter)
	}
}

func (f *Interface) sendPathCheckReply(ci *ConnectionState, endpoint *net.UDPAddr, counter []byte) {
	c := ci.messageCounter
    b := HeaderEncode(nil, Version, uint8(path_check_reply), 0, ci.remoteIndex, c)
	ci.messageCounter++

	if ci.eKey != nil {
		msg := ci.eKey.EncryptDanger(b, nil, counter, c)
		f.outside.WriteTo(msg, endpoint)
		l.Debugln("path_check sent, remote index: ", ci.remoteIndex)
	}
}
*/
