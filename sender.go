package zabbix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// Sender struct.
type Sender struct {
	Hosts          []string // ordered list of proxies/servers; first successful cached in PrimaryHost
	PrimaryHost    string   // cached working host (empty = round-robin first)
	MaxRedirects   int      // max redirect attempts bedore error; default is 3
	UpdateHost     bool     // if true, update s.Host to final proxy after success
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// NewSenderTimeout creates Sender with custom timeouts.
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
	res, err := io.ReadAll(conn)
	if err != nil {
		return res, fmt.Errorf("receiving data: %s", err.Error())
	}

	return res, nil
}

// SendMetrics sends mixed active+trapper metrics.
// Automatically separates into "agent data" and "sender data" packets.
// Returns 4 values: (activeRes, activeErr, trapperRes, trapperErr)
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

// Send sends single packet with redirect/HA handling.
// Caches working PrimaryHost for future calls.
func (s *Sender) Send(packet *Packet) (res Response, err error) {
	if s.PrimaryHost != "" {
		res, err = s.sendWithRedirects(packet, s.PrimaryHost)
		if err == nil {
			return res, nil
		}
		s.PrimaryHost = "" // clear cache
	}

	// Fallback: try each host in order
	for _, host := range s.Hosts {
		res, err = s.sendWithRedirects(packet, host)
		if err == nil {
			s.PrimaryHost = host // cache working host
			return res, nil
		}
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

// RegisterHost sends host autoregistration request ("active checks").
// Retries once as Zabbix requires 2 calls for confirmation.
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
