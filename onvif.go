package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	mediaXAddrRe = regexp.MustCompile(`(?s)<[\w:]*Media>.*?<[\w:]*XAddr>([^<]+)<`)
	profileRe    = regexp.MustCompile(`<[\w:]*Profiles[^>]*\stoken="([^"]+)"`)
	uriRe        = regexp.MustCompile(`<[\w:]*Uri>([^<]+)<`)
	faultRe      = regexp.MustCompile(`(?s)<[\w:]*Text[^>]*>([^<]+)<`)
)

var onvifClient = &http.Client{Timeout: 6 * time.Second}

// onvifStreamURI logs in to an ONVIF camera and asks it for the RTSP address
// of its first (main) stream, so the user does not need to know the path.
func onvifStreamURI(xaddr, user, pass string) (string, error) {
	dev, err := url.Parse(xaddr)
	if err != nil || (dev.Scheme != "http" && dev.Scheme != "https") {
		return "", errors.New("ONVIF 주소가 올바르지 않습니다")
	}
	// Only talk to devices on local networks: this endpoint must not become a
	// general-purpose request proxy.
	if ip := net.ParseIP(dev.Hostname()); ip == nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return "", errors.New("내부 네트워크의 카메라만 조회할 수 있습니다")
	}
	media := xaddr
	if body, err := onvifCall(xaddr, user, pass, `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>Media</Category></GetCapabilities>`); err == nil {
		if m := mediaXAddrRe.FindStringSubmatch(body); m != nil {
			// keep the address we can reach; cameras sometimes report another one
			if mu, err := url.Parse(strings.TrimSpace(m[1])); err == nil {
				mu.Scheme, mu.Host = dev.Scheme, dev.Host
				media = mu.String()
			}
		}
	}
	body, err := onvifCall(media, user, pass, `<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`)
	if err != nil {
		return "", err
	}
	m := profileRe.FindStringSubmatch(body)
	if m == nil {
		return "", errors.New("카메라에 영상 프로필이 없습니다")
	}
	body, err = onvifCall(media, user, pass, `<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><StreamSetup>`+
		`<Stream xmlns="http://www.onvif.org/ver10/schema">RTP-Unicast</Stream><Transport xmlns="http://www.onvif.org/ver10/schema"><Protocol>RTSP</Protocol></Transport>`+
		`</StreamSetup><ProfileToken>`+html.EscapeString(html.UnescapeString(m[1]))+`</ProfileToken></GetStreamUri>`)
	if err != nil {
		return "", err
	}
	um := uriRe.FindStringSubmatch(body)
	if um == nil {
		return "", errors.New("카메라가 영상 주소를 알려주지 않았습니다")
	}
	su, err := url.Parse(html.UnescapeString(strings.TrimSpace(um[1])))
	if err != nil || !strings.HasPrefix(su.Scheme, "rtsp") {
		return "", errors.New("카메라가 알려준 영상 주소를 해석할 수 없습니다")
	}
	port := su.Port()
	if port == "" {
		port = "554"
	}
	su.Host = net.JoinHostPort(dev.Hostname(), port)
	su.User = nil
	return su.String(), nil
}

func onvifCall(endpoint, user, pass, body string) (string, error) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	created := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	h := sha1.New() // required by the WS-Security UsernameToken profile
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	env := `<?xml version="1.0" encoding="UTF-8"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Header>` +
		`<Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"><UsernameToken>` +
		`<Username>` + html.EscapeString(user) + `</Username>` +
		`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + base64.StdEncoding.EncodeToString(h.Sum(nil)) + `</Password>` +
		`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
		`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>` +
		`</UsernameToken></Security></s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	resp, err := onvifClient.Post(endpoint, "application/soap+xml; charset=utf-8", strings.NewReader(env))
	if err != nil {
		return "", errors.New("카메라에 연결할 수 없습니다")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || strings.Contains(string(b), "NotAuthorized") {
			return "", errors.New("아이디 또는 비밀번호가 틀렸습니다")
		}
		if m := faultRe.FindStringSubmatch(string(b)); m != nil {
			return "", errors.New(strings.TrimSpace(m[1]))
		}
		return "", fmt.Errorf("카메라 응답 오류 (%d)", resp.StatusCode)
	}
	return string(b), nil
}
