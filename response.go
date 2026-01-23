package zabbix_sender

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// RedirectInfo struct.
type RedirectInfo struct {
	Revision int    `json:"revision"`
	Address  string `json:"address"`
}

// Response is the response struct from Zabbix server/proxy.
type Response struct {
	Response string        `json:"response"`
	Info     string        `json:"info"`
	Redirect *RedirectInfo `json:"redirect,omitempty"`
}

// ResponseInfo struct holds parsed statistics from response "info" field.
type ResponseInfo struct {
	Processed int
	Failed    int
	Total     int
	Spent     time.Duration
}

// parseHostPort validates and returns a normalized host:port address.
func parseHostPort(addr string) (string, error) {
	addr = normalizeHost(addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return "", fmt.Errorf("invalid redirect address: %s", addr)
	}
	return addr, nil
}

// normalizeHost ensures the address has a port; defaults to 10051 if missing.
func normalizeHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":10051"
}

// GetInfo parses success response "info" field into statistics.
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
