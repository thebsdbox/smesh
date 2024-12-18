package connection

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/cilium/ebpf"
	"github.com/gookit/slog"
)

type Config struct {
	ProxyPort      int
	ClusterPort    int
	ClusterTLSPort int
	Address        string
	ClusterAddress string // For Debug purposes
	CgroupOverride string // For Debug purposes

	PodCIDR      string
	Certificates *Certs

	Socks *ebpf.Map

	Proxy     bool
	ProxyFunc func(string) string
}

func (c *Config) StartInternalListener() net.Listener {
	proxyAddr := fmt.Sprintf("%s:%d", c.Address, c.ProxyPort)
	listener, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		slog.Fatalf("Failed to start proxy server: %v", err)
	}
	slog.Infof("[pid: %d] %s", os.Getpid(), proxyAddr)
	return listener
}

func (c *Config) StartExternalListener() net.Listener {
	proxyAddr := fmt.Sprintf("0.0.0.0:%d", c.ClusterPort)
	listener, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		slog.Fatalf("Failed to start proxy server: %v", err)
	}
	slog.Infof("[pid: %d] %s", os.Getpid(), proxyAddr)
	return listener
}

func (c *Config) StartExternalTLSListener() net.Listener {
	proxyAddr := fmt.Sprintf("0.0.0.0:%d", c.ClusterTLSPort)

	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(c.Certificates.ca) {
		log.Fatalf("could not append CA")
	}
	certificate, err := tls.X509KeyPair(c.Certificates.cert, c.Certificates.key)

	if err != nil {
		log.Fatalf("could not load certificate: %v", err)
	}

	config := &tls.Config{
		ClientCAs:    caCertPool,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.VerifyClientCertIfGiven,
	} //<-- this is the key

	listener, err := tls.Listen("tcp", proxyAddr, config)

	// listener, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		slog.Fatalf("Failed to start proxy server: %v", err)
	}
	slog.Infof("[pid: %d] %s", os.Getpid(), proxyAddr)
	return listener
}

// Blocking function
func (c *Config) StartListeners(listener net.Listener, internal bool) {
	for {
		conn, err := listener.Accept()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Printf("Failed to accept connection: %v", err)
			continue
		} else {
			if conn != nil {
				if internal {
					slog.Printf("internal %s -> %s", conn.RemoteAddr().String(), conn.LocalAddr().String())
					go c.internalProxy(conn)
				} else {
					slog.Printf("external %s -> %s", conn.RemoteAddr().String(), conn.LocalAddr().String())
					go c.handleExternalConnection(conn)
				}
			}
		}
	}
}

// Blocking function
func (c *Config) StartTLSListener(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) { // Don't print closing connection error, just continue to the next
				slog.Printf("Failed to accept connection: %v", err)
			}
			continue
		}

		slog.Printf(" %s -> %s", conn.RemoteAddr().String(), conn.LocalAddr().String())
		go c.handleTLSExternalConnection(conn)

	}
}

// HTTP proxy request handler
func (c *Config) internalProxy(conn net.Conn) {
	defer conn.Close()
	// Get original destination address
	destAddr, destPort, err := c.findTargetFromConnection(conn)
	if err != nil {
		return
	}
	targetDestination := fmt.Sprintf("%s:%d", destAddr, destPort)
	var targetConn net.Conn
	var endpoint string
	// Send traffic to endpoint gateway
	if c.Certificates != nil {

		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(c.Certificates.ca) {
			log.Fatalf("could not append CA")
		}
		certificate, err := tls.X509KeyPair(c.Certificates.cert, c.Certificates.key)
		if err != nil {
			log.Fatalf("could not load certificate: %v", err)
		}

		config := &tls.Config{
			RootCAs:      caCertPool,
			Certificates: []tls.Certificate{certificate},
			ClientAuth:   tls.VerifyClientCertIfGiven,
		} //<-- this is the key

		endpoint = fmt.Sprintf("%s:%d", destAddr, c.ClusterTLSPort)
		if c.ClusterAddress != "" {
			endpoint = fmt.Sprintf("%s:%d", c.ClusterAddress, c.ClusterPort)
		}
		if c.Proxy {
			endpoint = c.ProxyFunc(destAddr)
		}

		// Set a timeout, mainly because connections can occur to pods that aren't ready
		d := net.Dialer{Timeout: time.Second * 3}
		targetConn, err = tls.DialWithDialer(&d, "tcp", endpoint, config)
		if err != nil {
			slog.Printf("Failed to connect to destination TLS proxy: %v", err)
			return
		}
	} else {
		endpoint = fmt.Sprintf("%s:%d", destAddr, c.ClusterPort)
		if c.ClusterAddress != "" {
			endpoint = fmt.Sprintf("%s:%d", c.ClusterAddress, c.ClusterPort)
		}
		// Check that the original destination address is reachable from the proxy
		targetConn, err = net.DialTimeout("tcp", endpoint, 5*time.Second)
		if err != nil {
			slog.Printf("Failed to connect to original destination: %v", err)
			return
		}
	}
	defer targetConn.Close()

	slog.Printf("connect to proxy %s, original %s", endpoint, targetDestination)
	//log.Printf("Internal proxy sending original destination: %s\n", targetDestination)
	_, err = targetConn.Write([]byte(targetDestination))
	if err != nil {
		slog.Printf("Failed to send original destination: %v", err)
	}

	tmp := make([]byte, 256)

	// Ideally we wait here until our remote endpoint has recieved the targetDestination
	targetConn.Read(tmp)

	//log.Printf("Internal connection from %s to %s\n", conn.RemoteAddr(), targetConn.RemoteAddr())

	// The following code creates two data transfer channels:
	// - From the client to the target server (handled by a separate goroutine).
	// - From the target server to the client (handled by the main goroutine).
	go func() {
		_, err = io.Copy(targetConn, conn)
		if err != nil {
			slog.Printf("Failed copying data to target: %v", err)
		}
	}()
	_, err = io.Copy(conn, targetConn)
	if err != nil {
		slog.Printf("Failed copying data from target: %v", err)
	}
}

// Unencrypted external connection
func (c *Config) handleExternalConnection(conn net.Conn) {
	defer conn.Close()

	tmp := make([]byte, 256)
	n, err := conn.Read(tmp)
	if err != nil {
		slog.Print(err)
	}
	remoteAddress := string(tmp[:n])

	if remoteAddress == fmt.Sprintf("%s:%d", c.Address, c.ProxyPort) {
		slog.Printf("Potential loopback")
		return
	}

	// Check that the original destination address is reachable from the proxy
	targetConn, err := net.DialTimeout("tcp", remoteAddress, 5*time.Second)
	if err != nil {
		slog.Printf("Failed to connect to original destination[%s]: %v", string(tmp), err)
		return
	}
	defer targetConn.Close()
	conn.Write([]byte{'Y'}) // Send a response to kickstart the comms

	slog.Printf("%s -> %s", conn.RemoteAddr(), targetConn.RemoteAddr())

	// The following code creates two data transfer channels:
	// - From the client to the target server (handled by a separate goroutine).
	// - From the target server to the client (handled by the main goroutine).
	go func() {
		_, err = io.Copy(targetConn, conn)
		if err != nil {
			slog.Printf("Failed copying data to target: %v", err)
		}
	}()
	_, err = io.Copy(conn, targetConn)
	if err != nil {
		slog.Printf("Failed copying data from target: %v", err)
	}
}

// Unencrypted external connection
func (c *Config) handleTLSExternalConnection(conn net.Conn) {
	defer conn.Close()
	var tConn *tls.Conn = conn.(*tls.Conn)

	tmp := make([]byte, 256)
	n, err := tConn.Read(tmp)
	if err != nil {
		slog.Print(err)
	}

	remoteAddress := string(tmp[:n])

	if remoteAddress == fmt.Sprintf("%s:%d", c.Address, c.ProxyPort) {
		slog.Printf("Potential loopback")
		return
	}

	// Check that the original destination address is reachable from the proxy
	targetConn, err := net.DialTimeout("tcp", remoteAddress, 5*time.Second)
	//targetConn, err := tls.Dial("tcp", remoteAddress, config)
	if err != nil {
		slog.Printf("Failed to connect to original destination[%s]: %v", string(tmp), err)
		return
	}
	defer targetConn.Close()
	tConn.Write([]byte{'Y'}) // Send a response to kickstart the comms

	slog.Printf("%s -> %s", conn.RemoteAddr(), targetConn.RemoteAddr())

	// The following code creates two data transfer channels:
	// - From the client to the target server (handled by a separate goroutine).
	// - From the target server to the client (handled by the main goroutine).
	go func() {
		_, err = io.Copy(targetConn, tConn)
		if err != nil {
			slog.Printf("Failed copying data to target: %v", err)
		}
	}()
	_, err = io.Copy(tConn, targetConn)
	if err != nil {
		slog.Printf("Failed copying data from target: %v", err)
	}
}
