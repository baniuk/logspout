package syslog

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"log/syslog"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
)

const (
	// TraditionalTCPFraming is the traditional LF framing of syslog messages on the wire
	TraditionalTCPFraming TCPFraming = "traditional"
	// OctetCountedTCPFraming prepends the size of each message before the message. https://tools.ietf.org/html/rfc6587#section-3.4.1
	OctetCountedTCPFraming TCPFraming = "octet-counted"

	defaultRetryCount = 10
)

var (
	hostname         string
	retryCount       uint
	tcpFraming       TCPFraming
	econnResetErrStr string
)

// TCPFraming represents the type of framing to use for syslog messages
type TCPFraming string

func init() {
	hostname, _ = os.Hostname()
	econnResetErrStr = fmt.Sprintf("write: %s", syscall.ECONNRESET.Error())
	router.AdapterFactories.Register(NewSyslogAdapter, "syslog")
	setRetryCount()
}

func setRetryCount() {
	if count, err := strconv.Atoi(cfg.GetEnvDefault("RETRY_COUNT", strconv.Itoa(defaultRetryCount))); err != nil {
		retryCount = uint(defaultRetryCount)
	} else {
		retryCount = uint(count)
	}
	debug("setting retryCount to:", retryCount)
}

func debug(v ...interface{}) {
	if os.Getenv("DEBUG") != "" {
		log.Println(v...)
	}
}

func getHostname() string {
	content, err := ioutil.ReadFile("/etc/host_hostname")
	if err == nil && len(content) > 0 {
		hostname = strings.TrimRight(string(content), "\r\n")
	} else {
		hostname = cfg.GetEnvDefault("SYSLOG_HOSTNAME", "{{.Container.Config.Hostname}}")
	}
	return hostname
}

// NewSyslogAdapter returnas a configured syslog.Adapter
func NewSyslogAdapter(route *router.Route) (router.LogAdapter, error) {
	transport, found := router.AdapterTransports.Lookup(route.AdapterTransport("udp"))
	if !found {
		return nil, errors.New("bad transport: " + route.Adapter)
	}
	conn, err := transport.Dial(route.Address, route.Options)
	if err != nil {
		return nil, err
	}

	format := cfg.GetEnvDefault("SYSLOG_FORMAT", "rfc5424")
	priority := cfg.GetEnvDefault("SYSLOG_PRIORITY", "{{.Priority}}")
	pid := cfg.GetEnvDefault("SYSLOG_PID", "{{.Container.State.Pid}}")
	hostname = getHostname()

	tag := cfg.GetEnvDefault("SYSLOG_TAG", "{{.ContainerName}}"+route.Options["append_tag"])
	structuredData := cfg.GetEnvDefault("SYSLOG_STRUCTURED_DATA", "")
	if route.Options["structured_data"] != "" {
		structuredData = route.Options["structured_data"]
	}
	data := cfg.GetEnvDefault("SYSLOG_DATA", "{{.Data}}")
	timestamp := cfg.GetEnvDefault("SYSLOG_TIMESTAMP", "{{.Timestamp}}")

	if structuredData == "" {
		structuredData = "-"
	} else {
		structuredData = fmt.Sprintf("[%s]", structuredData)
	}

	if isTCPConnecion(conn) {
		if err = setTCPFraming(); err != nil {
			return nil, err
		}
	}

	var tmplStr string
	switch format {
	case "rfc5424":
		// notes from RFC:
		// - there is no upper limit for the entire message and depends on the transport in use
		// - the HOSTNAME field must not exceed 255 characters
		// - the TAG field must not exceed 48 characters
		// - the PROCID field must not exceed 128 characters
		tmplStr = fmt.Sprintf("<%s>1 %s %.255s %.48s %.128s - %s %s\n",
			priority, timestamp, hostname, tag, pid, structuredData, data)
	case "rfc3164":
		// notes from RFC:
		// - the entire message must be <= 1024 bytes
		// - the TAG field must not exceed 32 characters
		tmplStr = fmt.Sprintf("<%s>%s %s %.32s[%s]: %s\n",
			priority, timestamp, hostname, tag, pid, data)
	default:
		return nil, errors.New("unsupported syslog format: " + format)
	}
	tmpl, err := template.New("syslog").Parse(tmplStr)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		route:     route,
		conn:      conn,
		tmpl:      tmpl,
		transport: transport,
	}, nil
}

func setTCPFraming() error {
	switch s := cfg.GetEnvDefault("SYSLOG_TCP_FRAMING", "traditional"); s {
	case "traditional":
		tcpFraming = TraditionalTCPFraming
		return nil
	case "octet-counted":
		tcpFraming = OctetCountedTCPFraming
		return nil
	default:
		return fmt.Errorf("unknown SYSLOG_TCP_FRAMING value: %s", s)
	}
}

// Adapter streams log output to a connection in the Syslog format
type Adapter struct {
	conn      net.Conn
	route     *router.Route
	tmpl      *template.Template
	transport router.AdapterTransport
}

// Stream sends log data to a connection
func (a *Adapter) Stream(logstream chan *router.Message) {
	for message := range logstream {
		m := &Message{message}
		buf, err := m.Render(a.tmpl)
		if err != nil {
			log.Println("syslog:", err)
			return
		}

		if isTCPConnecion(a.conn) {
			switch tcpFraming {
			case OctetCountedTCPFraming:
				buf = append([]byte(fmt.Sprintf("%d ", len(buf))), buf...)
			case TraditionalTCPFraming:
				// leave as-is
			default:
				// should never get here, validated above
				panic("unknown framing format: " + tcpFraming)
			}
		}

		if _, err = a.conn.Write(buf); err != nil {
			log.Println("syslog:", err)
			switch a.conn.(type) {
			case *net.UDPConn:
				continue
			default:
				if err = a.retry(buf, err); err != nil {
					log.Panicf("syslog retry err: %+v", err)
					return
				}
			}
		}
	}
}

func (a *Adapter) retry(buf []byte, err error) error {
	if opError, ok := err.(*net.OpError); ok {
		if (opError.Temporary() && opError.Err.Error() != econnResetErrStr) || opError.Timeout() {
			retryErr := a.retryTemporary(buf)
			if retryErr == nil {
				return nil
			}
		}
	}
	if reconnErr := a.reconnect(); reconnErr != nil {
		return reconnErr
	}
	if _, err = a.conn.Write(buf); err != nil {
		log.Println("syslog: reconnect failed")
		return err
	}
	log.Println("syslog: reconnect successful")
	return nil
}

func (a *Adapter) retryTemporary(buf []byte) error {
	log.Printf("syslog: retrying tcp up to %v times\n", retryCount)
	err := retryExp(func() error {
		_, err := a.conn.Write(buf)
		if err == nil {
			log.Println("syslog: retry successful")
			return nil
		}

		return err
	}, retryCount)

	if err != nil {
		log.Println("syslog: retry failed")
		return err
	}

	return nil
}

func (a *Adapter) reconnect() error {
	log.Printf("syslog: reconnecting up to %v times\n", retryCount)
	err := retryExp(func() error {
		conn, err := a.transport.Dial(a.route.Address, a.route.Options)
		if err != nil {
			return err
		}
		a.conn = conn
		return nil
	}, retryCount)

	if err != nil {
		return err
	}
	return nil
}

func retryExp(fun func() error, tries uint) error {
	try := uint(0)
	for {
		err := fun()
		if err == nil {
			return nil
		}

		try++
		if try > tries {
			return err
		}

		time.Sleep((1 << try) * 10 * time.Millisecond)
	}
}

func isTCPConnecion(conn net.Conn) bool {
	switch conn.(type) {
	case *net.TCPConn:
		return true
	case *tls.Conn:
		return true
	default:
		return false
	}
}

// Message extends router.Message for the syslog standard
type Message struct {
	*router.Message
}

// Render transforms the log message using the Syslog template
func (m *Message) Render(tmpl *template.Template) ([]byte, error) {
	buf := new(bytes.Buffer)
	err := tmpl.Execute(buf, m)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Priority returns a syslog.Priority based on the message source
func (m *Message) Priority() syslog.Priority {
	switch m.Message.Source {
	case "stdout":
		return syslog.LOG_USER | syslog.LOG_INFO
	case "stderr":
		return syslog.LOG_USER | syslog.LOG_ERR
	default:
		return syslog.LOG_DAEMON | syslog.LOG_INFO
	}
}

// Hostname returns the os hostname
func (m *Message) Hostname() string {
	return hostname
}

// Timestamp returns the message's syslog formatted timestamp
func (m *Message) Timestamp() string {
	return m.Message.Time.Format(time.RFC3339)
}

// ContainerName returns the message's container name
func (m *Message) ContainerName() string {
	return m.Message.Container.Name[1:]
}

// ContainerNameSplitN returns the message's container name sliced at most "n" times using "sep"
func (m *Message) ContainerNameSplitN(sep string, n int) []string {
	return strings.SplitN(m.ContainerName(), sep, n)
}
