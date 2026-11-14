package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Camera discovery. Several independent methods run at the same time:
//
//	usb       capture devices on this computer (via ffmpeg)
//	onvif     WS-Discovery multicast: the standard nearly every IP camera supports
//	hikvision SADP multicast: cameras answer on the multicast group itself, so
//	          they are found even when their IP is in a different subnet
//	dahua     DHDiscover multicast/broadcast: same idea for Dahua-based cameras
//	scan      TCP scan of the local subnets for RTSP servers and phone camera
//	          apps (IP Webcam :8080, DroidCam :4747), which have no discovery protocol
const discoverWindow = 3 * time.Second

type NetInfo struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
	CIDR string `json:"cidr"`
}

type Found struct {
	Kind       string   `json:"kind"` // usb | hikvision | dahua | onvif | ipwebcam | droidcam | rtsp
	Name       string   `json:"name"`
	IP         string   `json:"ip,omitempty"`
	Mask       string   `json:"mask,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Model      string   `json:"model,omitempty"`
	Port       int      `json:"port,omitempty"`
	XAddr      string   `json:"xaddr,omitempty"` // ONVIF service address, for stream lookup
	FoundBy    []string `json:"foundBy"`
	SameSubnet bool     `json:"sameSubnet"`
	Reachable  bool     `json:"reachable"`
	Status     string   `json:"status"` // ok | routed | unreachable | noport
	Message    string   `json:"message"`
	NeedsLogin bool     `json:"needsLogin"`
	Draft      Camera   `json:"draft"`
}

type localNet struct {
	Name  string
	IP    net.IP
	Net   *net.IPNet
	Iface net.Interface
}

func localNets() []localNet {
	var out []localNet
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, localNet{ifc.Name, ip, &net.IPNet{IP: ip.Mask(ipn.Mask), Mask: ipn.Mask}, ifc})
		}
	}
	return out
}

var kindRank = map[string]int{"hikvision": 5, "dahua": 5, "onvif": 4, "ipwebcam": 3, "droidcam": 3, "rtsp": 1}

type collector struct {
	mu sync.Mutex
	m  map[string]*Found
}

// add merges a sighting into the result, keyed by IP. The most specific
// method wins the "kind"; details from the others fill the gaps.
func (c *collector) add(f Found, by string) {
	if net.ParseIP(f.IP).To4() == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.m[f.IP]
	if !ok {
		f.FoundBy = []string{by}
		c.m[f.IP] = &f
		return
	}
	seen := false
	for _, b := range cur.FoundBy {
		seen = seen || b == by
	}
	if !seen {
		cur.FoundBy = append(cur.FoundBy, by)
	}
	if kindRank[f.Kind] > kindRank[cur.Kind] {
		cur.Kind = f.Kind
		if f.Port != 0 {
			cur.Port = f.Port
		}
	}
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&cur.Name, f.Name)
	fill(&cur.Model, f.Model)
	fill(&cur.Mask, f.Mask)
	fill(&cur.Gateway, f.Gateway)
	fill(&cur.MAC, f.MAC)
	fill(&cur.XAddr, f.XAddr)
	if cur.Port == 0 {
		cur.Port = f.Port
	}
}

// udpProbe sends payload to dst from the given interface and passes every
// reply to handle until the deadline. If group is set it also listens on that
// multicast group, where some vendors send their answers.
func udpProbe(ln localNet, dst, group *net.UDPAddr, payload []byte, deadline time.Time, handle func([]byte, *net.UDPAddr)) {
	var wg sync.WaitGroup
	read := func(conn *net.UDPConn) {
		defer wg.Done()
		defer conn.Close()
		conn.SetReadDeadline(deadline)
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			handle(append([]byte(nil), buf[:n]...), from)
		}
	}
	if group != nil {
		if g, err := net.ListenMulticastUDP("udp4", &ln.Iface, group); err == nil {
			multicastLoop(g)
			wg.Add(1)
			go read(g)
		}
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ln.IP})
	if err == nil {
		wg.Add(1)
		go read(conn)
		for i := 0; i < 2; i++ { // UDP is lossy: ask twice
			conn.WriteToUDP(payload, dst)
			time.Sleep(400 * time.Millisecond)
		}
	}
	wg.Wait()
}

/* ---------- ONVIF WS-Discovery ---------- */

var (
	xaddrsRe = regexp.MustCompile(`XAddrs>([^<]+)<`)
	scopesRe = regexp.MustCompile(`Scopes>([^<]+)<`)
)

func probeONVIF(ln localNet, deadline time.Time, c *collector) {
	msg := `<?xml version="1.0" encoding="UTF-8"?><e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl"><e:Header><w:MessageID>uuid:` +
		uuid() + `</w:MessageID><w:To e:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To><w:Action e:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action></e:Header><e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body></e:Envelope>`
	udpProbe(ln, &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}, nil, []byte(msg), deadline, func(b []byte, from *net.UDPAddr) {
		if f, ok := parseONVIF(string(b), from.IP.String()); ok {
			c.add(f, "ONVIF")
		}
	})
}

func parseONVIF(s, fromIP string) (Found, bool) {
	if !strings.Contains(s, "ProbeMatch") {
		return Found{}, false
	}
	f := Found{Kind: "onvif", IP: fromIP}
	if m := xaddrsRe.FindStringSubmatch(s); m != nil {
		for _, x := range strings.Fields(m[1]) {
			if u, err := url.Parse(x); err == nil && net.ParseIP(u.Hostname()).To4() != nil {
				f.XAddr, f.IP = x, u.Hostname()
				break
			}
		}
	}
	if m := scopesRe.FindStringSubmatch(s); m != nil {
		for _, sc := range strings.Fields(m[1]) {
			if v, ok := strings.CutPrefix(sc, "onvif://www.onvif.org/name/"); ok {
				f.Name, _ = url.PathUnescape(v)
			} else if v, ok := strings.CutPrefix(sc, "onvif://www.onvif.org/hardware/"); ok {
				f.Model, _ = url.PathUnescape(v)
			}
		}
	}
	return f, true
}

/* ---------- Hikvision SADP ---------- */

type sadpMatch struct {
	XMLName     xml.Name
	Description string `xml:"DeviceDescription"`
	MAC         string `xml:"MAC"`
	IP          string `xml:"IPv4Address"`
	Mask        string `xml:"IPv4SubnetMask"`
	Gateway     string `xml:"IPv4Gateway"`
}

func probeSADP(ln localNet, deadline time.Time, c *collector) {
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 37020}
	msg := `<?xml version="1.0" encoding="utf-8"?><Probe><Uuid>` + strings.ToUpper(uuid()) + `</Uuid><Types>inquiry</Types></Probe>`
	udpProbe(ln, group, group, []byte(msg), deadline, func(b []byte, _ *net.UDPAddr) {
		if f, ok := parseSADP(b); ok {
			c.add(f, "Hikvision SADP")
		}
	})
}

func parseSADP(b []byte) (Found, bool) {
	var m sadpMatch
	if xml.Unmarshal(b, &m) != nil || m.XMLName.Local != "ProbeMatch" || m.IP == "" {
		return Found{}, false
	}
	return Found{Kind: "hikvision", IP: m.IP, Mask: m.Mask, Gateway: m.Gateway,
		MAC: strings.ToUpper(strings.ReplaceAll(m.MAC, "-", ":")), Model: m.Description, Port: 554}, true
}

/* ---------- Dahua DHDiscover ---------- */

func probeDahua(ln localNet, deadline time.Time, c *collector) {
	body := []byte(`{"method":"DHDiscover.search","params":{"mac":"","uni":1}}`)
	pkt := make([]byte, 32, 32+len(body))
	copy(pkt, []byte{0x20, 0, 0, 0, 'D', 'H', 'I', 'P'})
	binary.LittleEndian.PutUint32(pkt[16:], uint32(len(body)))
	binary.LittleEndian.PutUint32(pkt[24:], uint32(len(body)))
	pkt = append(pkt, body...)
	handle := func(b []byte, _ *net.UDPAddr) {
		if f, ok := parseDahua(b); ok {
			c.add(f, "Dahua")
		}
	}
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 251), Port: 37810}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); udpProbe(ln, group, group, pkt, deadline, handle) }()
	go func() { // older firmware answers a plain broadcast instead
		defer wg.Done()
		udpProbe(ln, &net.UDPAddr{IP: net.IPv4bcast, Port: 5050}, nil, pkt, deadline, handle)
	}()
	wg.Wait()
}

func parseDahua(b []byte) (Found, bool) {
	i := strings.IndexByte(string(b), '{')
	if i < 0 {
		return Found{}, false
	}
	var m struct {
		MAC    string `json:"mac"`
		Params struct {
			DeviceInfo struct {
				DeviceType  string
				MachineName string
				IPv4Address struct{ IPAddress, SubnetMask, DefaultGateway string }
			} `json:"deviceInfo"`
		} `json:"params"`
	}
	if json.NewDecoder(strings.NewReader(string(b[i:]))).Decode(&m) != nil {
		return Found{}, false
	}
	d := m.Params.DeviceInfo
	if d.IPv4Address.IPAddress == "" {
		return Found{}, false
	}
	return Found{Kind: "dahua", IP: d.IPv4Address.IPAddress, Mask: d.IPv4Address.SubnetMask,
		Gateway: d.IPv4Address.DefaultGateway, MAC: strings.ToUpper(m.MAC), Model: d.DeviceType, Name: d.MachineName, Port: 554}, true
}

/* ---------- TCP scan of the local subnets ---------- */

var scanPorts = []int{554, 8554, 8080, 4747}

func scanHosts(ln localNet) []net.IP {
	if !ln.IP.IsPrivate() {
		return nil // never scan a public range
	}
	base, mask := binary.BigEndian.Uint32(ln.Net.IP.To4()), binary.BigEndian.Uint32(ln.Net.Mask)
	if ones, _ := ln.Net.Mask.Size(); ones < 24 { // large network: only our own /24
		mask = 0xffffff00
		base = binary.BigEndian.Uint32(ln.IP) & mask
	}
	var out []net.IP
	for h := base + 1; h < base|^mask; h++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, h)
		out = append(out, ip)
	}
	return out
}

func scanNet(ln localNet, c *collector) {
	sem := make(chan struct{}, 256)
	var wg sync.WaitGroup
	for _, ip := range scanHosts(ln) {
		for _, port := range scanPorts {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if f, ok := identify(ip.String(), port); ok {
					c.add(f, "네트워크 스캔")
				}
			}()
		}
	}
	wg.Wait()
}

// identify connects to one address and reports it only when it positively
// looks like a camera, so ordinary web servers are not listed.
func identify(ip string, port int) (Found, bool) {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return Found{}, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	if port == 554 || port == 8554 {
		fmt.Fprintf(conn, "OPTIONS rtsp://%s/ RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: mySurveillance\r\n\r\n", addr)
		head := readSome(conn, 1024, true)
		if !strings.HasPrefix(head, "RTSP/") {
			return Found{}, false
		}
		return Found{Kind: "rtsp", IP: ip, Port: port, Model: headerValue(head, "Server")}, true
	}
	fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\nUser-Agent: mySurveillance\r\n\r\n", addr)
	page := readSome(conn, 8192, false)
	switch {
	case strings.Contains(page, "IP Webcam"):
		return Found{Kind: "ipwebcam", IP: ip, Port: port, Name: "IP Webcam (폰)"}, true
	case strings.Contains(page, "DroidCam"):
		return Found{Kind: "droidcam", IP: ip, Port: port, Name: "DroidCam (폰)"}, true
	}
	return Found{}, false
}

// readSome reads until the response headers are complete (headersOnly), the
// peer closes, the deadline passes or max bytes arrived.
func readSome(r io.Reader, max int, headersOnly bool) string {
	buf := make([]byte, max)
	n := 0
	for n < max {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil || (headersOnly && strings.Contains(string(buf[:n]), "\r\n\r\n")) {
			break
		}
	}
	return string(buf[:n])
}

func headerValue(head, key string) string {
	sc := bufio.NewScanner(strings.NewReader(head))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

/* ---------- orchestration ---------- */

func uuid() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func discover(ffmpeg string) ([]NetInfo, []Found) {
	nets := localNets()
	c := &collector{m: map[string]*Found{}}
	deadline := time.Now().Add(discoverWindow)
	var usb Devices
	var wg sync.WaitGroup
	run := func(fn func()) { wg.Add(1); go func() { defer wg.Done(); fn() }() }
	if ffmpeg != "" {
		run(func() { usb = listDevices(ffmpeg) })
	}
	for _, ln := range nets {
		run(func() { probeONVIF(ln, deadline, c) })
		run(func() { probeSADP(ln, deadline, c) })
		run(func() { probeDahua(ln, deadline, c) })
		run(func() { scanNet(ln, c) })
	}
	wg.Wait()

	for _, f := range c.m {
		run(func() { finish(f, nets) })
	}
	wg.Wait()
	var found []Found
	for _, f := range c.m {
		found = append(found, *f)
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := net.ParseIP(found[i].IP).To4(), net.ParseIP(found[j].IP).To4()
		return binary.BigEndian.Uint32(a) < binary.BigEndian.Uint32(b)
	})

	out := []Found{}
	for _, v := range usb.Video {
		d := Camera{Name: v, Type: "usb", Enabled: true, VideoDevice: v, Audio: len(usb.Audio) > 0}
		if len(usb.Audio) > 0 {
			d.AudioDevice = usb.Audio[0]
		}
		out = append(out, Found{Kind: "usb", Name: v, FoundBy: []string{"USB"}, SameSubnet: true, Reachable: true,
			Status: "ok", Message: "이 컴퓨터에 연결된 카메라입니다.", Draft: d})
	}
	infos := []NetInfo{}
	for _, ln := range nets {
		infos = append(infos, NetInfo{ln.Name, ln.IP.String(), ln.Net.String()})
	}
	return infos, append(out, found...)
}

// finish decides whether the server can actually reach the device and
// prepares the camera draft the UI registers.
func finish(f *Found, nets []localNet) {
	ip := net.ParseIP(f.IP)
	var same *localNet
	for i := range nets {
		if nets[i].Net.Contains(ip) {
			same = &nets[i]
		}
	}
	f.SameSubnet = same != nil
	if f.Port == 0 {
		f.Port = 554
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(f.IP, strconv.Itoa(f.Port)), 700*time.Millisecond); err == nil {
		conn.Close()
		f.Reachable = true
	}
	var mine []string
	for _, ln := range nets {
		mine = append(mine, ln.Net.String())
	}
	camNet := f.IP
	if m := net.ParseIP(f.Mask).To4(); m != nil {
		ones, _ := net.IPMask(m).Size()
		camNet = fmt.Sprintf("%s/%d", f.IP, ones)
	}
	switch {
	case f.Reachable && f.SameSubnet:
		f.Status, f.Message = "ok", fmt.Sprintf("연결 가능 — 이 컴퓨터(%s)와 같은 대역입니다.", same.IP)
	case f.Reachable:
		f.Status, f.Message = "routed", "연결 가능 — 대역은 다르지만 공유기를 거쳐 연결됩니다."
	case f.SameSubnet:
		f.Status, f.Message = "noport", fmt.Sprintf("같은 대역이지만 영상 포트(%d)가 응답하지 않습니다. 카메라의 RTSP 설정을 확인하세요.", f.Port)
	default:
		f.Status = "unreachable"
		f.Message = fmt.Sprintf("연결 불가 — 카메라는 %s, 이 컴퓨터는 %s 대역입니다. 카메라 IP를 이 컴퓨터 대역으로 바꾸거나, 이 컴퓨터에 카메라 대역의 IP를 추가해야 녹화할 수 있습니다.",
			camNet, strings.Join(mine, ", "))
	}

	if f.Name == "" {
		f.Name = f.Model
	}
	if f.Name == "" {
		f.Name = "카메라 " + f.IP
	}
	host := net.JoinHostPort(f.IP, strconv.Itoa(f.Port))
	// several cameras of one model share a name; the IP keeps drafts apart
	draftName := f.Name
	if !strings.Contains(draftName, f.IP) {
		draftName += " (" + f.IP + ")"
	}
	d := Camera{Name: draftName, Enabled: true, Audio: true, Type: "rtsp"}
	switch f.Kind {
	case "hikvision":
		d.URL, f.NeedsLogin = "rtsp://"+host+"/Streaming/Channels/101", true
	case "dahua":
		d.URL, f.NeedsLogin = "rtsp://"+host+"/cam/realmonitor?channel=1&subtype=0", true
	case "ipwebcam":
		d.Type, d.URL, d.AudioURL = "http", "http://"+host+"/video", "http://"+host+"/audio.wav"
	case "droidcam":
		d.Type, d.URL, d.Audio = "http", "http://"+host+"/video", false
	default: // onvif, rtsp: the stream path is device specific
		d.URL, f.NeedsLogin = "rtsp://"+host+"/", true
	}
	f.Draft = d
}

var discoverMu sync.Mutex

func (a *App) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if !discoverMu.TryLock() {
		writeErr(w, http.StatusTooManyRequests, "이미 검색 중입니다. 잠시 뒤 다시 시도하세요")
		return
	}
	defer discoverMu.Unlock()
	nets, devs := discover(a.ffmpeg)
	writeJSON(w, 200, map[string]any{"networks": nets, "devices": devs})
}

func (a *App) handleONVIFStream(w http.ResponseWriter, r *http.Request) {
	var req struct{ XAddr, Username, Password string }
	if !readJSON(w, r, &req) {
		return
	}
	u, err := onvifStreamURI(req.XAddr, req.Username, req.Password)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "카메라에서 영상 주소를 받지 못했습니다: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"url": u})
}
