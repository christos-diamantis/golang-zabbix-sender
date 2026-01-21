// Package zabbix implements the sender protocol to send values to zabbix
// Taken from github.com/blacked/go-zabbix (discontinued)
package zabbix

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultConnectTimeout = 5 * time.Second
	defaultWriteTimeout   = 5 * time.Second
	defaultReadTimeout    = 15 * time.Second
	defaultMaxRedirects   = 3
	defaultUpdateHost     = false
)

// Metric class.
type Metric struct {
	Host   string `json:"host"`
	Key    string `json:"key"`
	Value  string `json:"value"`
	Clock  int64  `json:"clock,omitempty"`
	NS     int    `json:"ns,omitempty"`
	Active bool   `json:"-"`
}

// RedirectInfo class.
type RedirectInfo struct {
	Revision int    `json:"revision"`
	Address  string `json:"address"`
}

func parseHostPort(addr string) (string, error) {
	if !strings.Contains(addr, ":") {
		return "", fmt.Errorf("invalid redirect address: %s", addr) // maybe extend this to add the port?
	}
	return addr, nil
}

// NewMetric return a zabbix Metric with the values specified
// agentActive should be set to true if we are sending to a Zabbix Agent (active) item
func NewMetric(host, key, value string, agentActive bool, t ...time.Time) *Metric {
	m := &Metric{Host: host, Key: key, Value: value, Active: agentActive}
	if len(t) > 0 {
		m.Clock = t[0].Unix()
		m.NS = t[0].Nanosecond()
	}
	return m
}

// Packet class.
type Packet struct {
	Request      string    `json:"request"`
	Data         []*Metric `json:"data,omitempty"`
	Clock        int64     `json:"clock,omitempty"`
	NS           int       `json:"ns,omitempty"`
	Host         string    `json:"host,omitempty"`
	HostMetadata string    `json:"host_metadata,omitempty"`
}

// Reponse is a response for autoregister method
type Response struct {
	Response string        `json:"response"`
	Info     string        `json:"info"`
	Redirect *RedirectInfo `json:"redirect,omitempty"`
}

type ResponseInfo struct {
	Processed int
	Failed    int
	Total     int
	Spent     time.Duration
}

func (r *Response) GetInfo() (*ResponseInfo, error) {
	ret := new(ResponseInfo)

	if r.Response != "success" {
		return nil, fmt.Errorf("Can not process info if response not Success (%s)", r.Response)
	}

	sp := strings.Split(r.Info, ";")
	if len(sp) != 4 {
		return nil, fmt.Errorf("Error in splited data, expected 4 got %d for data (%s)", len(sp), r.Info)
	}
	for i := range sp {
		sp2 := strings.Split(sp[i], ":")
		if len(sp2) != 2 {
			return nil, fmt.Errorf("Error in splited data, expected 2 got %d for data (%s)", len(sp2), sp[i])
		}
		key := strings.TrimSpace(sp2[0])
		value := strings.TrimSpace(sp2[1])
		var err error
		switch key {
		case "processed":
			ret.Processed, err = strconv.Atoi(value)
		case "failed":
			ret.Failed, err = strconv.Atoi(value)
		case "total":
			ret.Total, err = strconv.Atoi(value)
		case "seconds spent":
			var f float64
			if f, err = strconv.ParseFloat(value, 64); err != nil {
				return nil, fmt.Errorf("Error in parsing seconds spent value [%s] error: %s", value, err)
			}
			ret.Spent = time.Duration(int64(f * 1000000000.0))
		}

	}

	return ret, nil
}

// NewPacket return a zabbix packet with a list of metrics
func NewPacket(data []*Metric, agentActive bool, t ...time.Time) *Packet {
	var request string
	if agentActive {
		request = "agent data"
	} else {
		request = "sender data"
	}

	p := &Packet{Request: request, Data: data}
	if len(t) > 0 {
		p.Clock = t[0].Unix()
		p.NS = t[0].Nanosecond()
	}
	return p
}

// DataLen Packet class method, return 8 bytes with packet length in little endian order
func (p *Packet) DataLen() []byte {
	dataLen := make([]byte, 8)
	JSONData, _ := json.Marshal(p)
	binary.LittleEndian.PutUint32(dataLen, uint32(len(JSONData)))
	return dataLen
}

// Sender class
type Sender struct {
	Hosts          []string // ordered list of proxies/servers; first successful cached in PrimaryHost
	PrimaryHost    string   // cached working host (empty = round-robin first)
	MaxRedirects   int      // max redirect attempts bedore error; default is 3
	UpdateHost     bool     // if true, update s.Host to final proxy after success
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// NewSender return a sender object to send metrics using default values for timeouts
func NewSender(host string) *Sender {
	return &Sender{
		Hosts:          []string{host},
		MaxRedirects:   defaultMaxRedirects,
		UpdateHost:     defaultUpdateHost,
		ConnectTimeout: defaultConnectTimeout,
		ReadTimeout:    defaultReadTimeout,
		WriteTimeout:   defaultWriteTimeout,
	}
}

func NewSenderHosts(hosts []string) *Sender {
	return &Sender{
		Hosts:          hosts,
		MaxRedirects:   defaultMaxRedirects,
		UpdateHost:     defaultUpdateHost,
		ConnectTimeout: defaultConnectTimeout,
		ReadTimeout:    defaultReadTimeout,
		WriteTimeout:   defaultWriteTimeout,
	}
}

// NewSenderTimeout return a sender object to send metrics defining values for timeouts
func NewSenderTimeout(
	host string,
	connectTimeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) *Sender {
	return &Sender{
		Hosts:          []string{host},
		MaxRedirects:   defaultMaxRedirects,
		UpdateHost:     defaultUpdateHost,
		ConnectTimeout: connectTimeout,
		ReadTimeout:    readTimeout,
		WriteTimeout:   writeTimeout,
	}
}

// getHeader return zabbix header.
// https://www.zabbix.com/documentation/4.0/manual/appendix/protocols/header_datalen
func (s *Sender) getHeader() []byte {
	return []byte("ZBXD\x01")
}

// read data from connection.
func (s *Sender) read(conn net.Conn) ([]byte, error) {
	res, err := ioutil.ReadAll(conn)
	if err != nil {
		return res, fmt.Errorf("receiving data: %s", err.Error())
	}

	return res, nil
}

// SendMetrics send an array of metrics, making different packets for
// trapper and active items.
// The response for trapper metrics is in the first element of the res array and err array
// Response for active metrics is in the second element of the res array and error array
func (s *Sender) SendMetrics(metrics []*Metric) (resActive Response, errActive error, resTrapper Response, errTrapper error) {
	var trapperMetrics []*Metric
	var activeMetrics []*Metric

	for i := range metrics {
		if metrics[i].Active {
			activeMetrics = append(activeMetrics, metrics[i])
		} else {
			trapperMetrics = append(trapperMetrics, metrics[i])
		}
	}

	if len(trapperMetrics) > 0 {

		packetTrapper := NewPacket(trapperMetrics, false)
		resTrapper, errTrapper = s.Send(packetTrapper)
	}

	if len(activeMetrics) > 0 {
		packetActive := NewPacket(activeMetrics, true)
		resActive, errActive = s.Send(packetActive)
	}

	return resActive, errActive, resTrapper, errTrapper
}

// Send connects to Zabbix, send the data, return the response and close the connection
func (s *Sender) Send(packet *Packet) (res Response, err error) {
	if s.PrimaryHost != "" {
		res, err = s.sendWithRedirects(packet, s.PrimaryHost)
		if err == nil {
			return res, nil
		}
		fmt.Printf("Cached host %s failed: %v\n", s.PrimaryHost, err)
		s.PrimaryHost = "" // clear cache
	}

	// Fallback: try each host in order
	for i, host := range s.Hosts {
		fmt.Printf("Trying host %d/%d: %s\n", i+1, len(s.Hosts), host)
		res, err = s.sendWithRedirects(packet, host)
		if err == nil {
			s.PrimaryHost = host // cache working host
			return res, nil
		}
		fmt.Printf("Host %s failed: %v\n", host, err)
	}
	return res, fmt.Errorf("all %d hosts failed", len(s.Hosts))
}

func (s *Sender) sendWithRedirects(packet *Packet, startHost string) (res Response, err error) {

	currentHost := startHost

	for redirectCount := 0; redirectCount <= s.MaxRedirects; redirectCount++ {
		res, err = s.sendOnce(packet, currentHost)
		if err != nil {
			return res, fmt.Errorf("sendOnce to %s failed: %w", currentHost, err)
		}

		// success - done
		if res.Response == "success" {
			if s.UpdateHost && currentHost != startHost {
				fmt.Printf("Updated to final proxy: %s\n", currentHost)
			}
			return res, nil
		}

		// check for redirect
		if res.Redirect == nil || res.Redirect.Address == "" {
			return res, fmt.Errorf("failed without redirect from %s: %s", currentHost, res.Response)
		}

		// got redirect - update target and retry
		newHost, err := parseHostPort(res.Redirect.Address)
		if err != nil {
			return res, err
		}
		currentHost = newHost
		fmt.Printf("Redirect #%d to %s\n", redirectCount+1, currentHost)
	}

	return res, fmt.Errorf("max redirects exceeded from %s", startHost)
}

func (s *Sender) sendOnce(packet *Packet, host string) (res Response, err error) {
	// Timeout to resolve and connect to the server
	conn, err := net.DialTimeout("tcp", host, s.ConnectTimeout)
	if err != nil {
		return res, fmt.Errorf("connecting to %s (timeout=%v): %v", host, s.ConnectTimeout, err)
	}
	defer conn.Close()

	dataPacket, _ := json.Marshal(packet)

	// Fill buffer
	buffer := append(s.getHeader(), packet.DataLen()...)
	buffer = append(buffer, dataPacket...)

	// Write timeout
	conn.SetWriteDeadline(time.Now().Add(s.WriteTimeout))

	// Send packet to zabbix
	if _, err = conn.Write(buffer); err != nil {
		return res, fmt.Errorf("sending the data to %s (timeout=%v): %s", host, s.WriteTimeout, err.Error())
	}

	// Read timeout
	conn.SetReadDeadline(time.Now().Add(s.ReadTimeout))

	// Read response from server
	response, err := s.read(conn)
	if err != nil {
		return res, fmt.Errorf("reading the response from %s (timeout=%v): %s", host, s.ReadTimeout, err)
	}

	if len(response) < 13 {
		return res, fmt.Errorf("response too short from %s: %d bytes", host, len(response))
	}

	header := response[:5]
	data := response[13:]

	if !bytes.Equal(header, s.getHeader()) {
		return res, fmt.Errorf("got no valid header [%+v] , expected [%+v]", header, s.getHeader())
	}

	if err := json.Unmarshal(data, &res); err != nil {
		return res, fmt.Errorf("zabbix response from %s is not valid: %v", host, err)
	}

	return res, nil
}

// RegisterHost provides a register a Zabbix's host with Autoregister method.
func (s *Sender) RegisterHost(host, hostmetadata string) error {

	p := &Packet{Request: "active checks", Host: host, HostMetadata: hostmetadata}

	res, err := s.Send(p)
	if err != nil {
		return fmt.Errorf("sending packet: %v", err)
	}

	if res.Response == "success" {
		return nil
	}

	// The autoregister process always return fail the first time
	// We retry the process to get success response to verify the host registration properly
	p = &Packet{Request: "active checks", Host: host, HostMetadata: hostmetadata}

	res, err = s.Send(p)
	if err != nil {
		return fmt.Errorf("sending packet: %v", err)
	}

	if res.Response == "failed" {
		return fmt.Errorf("autoregistration failed, verify hostmetadata")
	}

	return nil
}
