package proxy

import (
	"crypto/tls"
	"github.com/grepplabs/kafka-proxy/config"
	"github.com/grepplabs/kafka-proxy/plugin/local-auth/shared"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"net"
	"sync"
	"time"
)

// Conn represents a connection from a client to a specific instance.
type Conn struct {
	BrokerAddress   string
	LocalConnection net.Conn
}

// Client is a type to handle connecting to a Server. All fields are required
// unless otherwise specified.
type Client struct {
	conns *ConnSet

	// Kafka Net configuration
	config *config.Config

	// Config of Proxy request-response processor (instance p)
	processorConfig ProcessorConfig

	tlsConfig      *tls.Config
	tcpConnOptions TCPConnOptions

	stopRun  chan struct{}
	stopOnce sync.Once
}

func NewClient(conns *ConnSet, c *config.Config, netAddressMappingFunc config.NetAddressMappingFunc, passwordAuthenticator shared.PasswordAuthenticator) (*Client, error) {
	tlsConfig, err := newTLSClientConfig(c)
	if err != nil {
		return nil, err
	}
	tcpConnOptions := TCPConnOptions{
		KeepAlive:       c.Kafka.KeepAlive,
		WriteBufferSize: c.Kafka.ConnectionWriteBufferSize,
		ReadBufferSize:  c.Kafka.ConnectionReadBufferSize,
	}

	forbiddenApiKeys := make(map[int16]struct{})
	if len(c.Kafka.ForbiddenApiKeys) != 0 {
		logrus.Warnf("Kafka operations for Api Keys %v will be forbidden.", c.Kafka.ForbiddenApiKeys)
		for _, apiKey := range c.Kafka.ForbiddenApiKeys {
			forbiddenApiKeys[int16(apiKey)] = struct{}{}
		}
	}

	return &Client{conns: conns, config: c, tlsConfig: tlsConfig, tcpConnOptions: tcpConnOptions, stopRun: make(chan struct{}, 1),
		processorConfig: ProcessorConfig{
			MaxOpenRequests:       c.Kafka.MaxOpenRequests,
			NetAddressMappingFunc: netAddressMappingFunc,
			RequestBufferSize:     c.Proxy.RequestBufferSize,
			ResponseBufferSize:    c.Proxy.ResponseBufferSize,
			ReadTimeout:           c.Kafka.ReadTimeout,
			WriteTimeout:          c.Kafka.WriteTimeout,
			LocalAuth:             c.Auth.Local.Enable,
			LocalAuthenticator:    passwordAuthenticator,
			ForbiddenApiKeys:      forbiddenApiKeys,
		}}, nil
}

// Run causes the client to start waiting for new connections to connSrc and
// proxy them to the destination instance. It blocks until connSrc is closed.
func (c *Client) Run(connSrc <-chan Conn) error {
STOP:
	for {
		select {
		case conn := <-connSrc:
			go withRecover(func() { c.handleConn(conn) })
		case <-c.stopRun:
			break STOP
		}
	}

	logrus.Info("Closing connections")

	if err := c.conns.Close(); err != nil {
		logrus.Infof("closing client had error: %v", err)
	}

	logrus.Info("Proxy is stopped")
	return nil
}

func (c *Client) Close() {
	c.stopOnce.Do(func() {
		close(c.stopRun)
	})
}

func (c *Client) handleConn(conn Conn) {
	proxyConnectionsTotal.WithLabelValues(conn.BrokerAddress).Inc()

	server, err := c.DialAndAuth(conn.BrokerAddress, c.tlsConfig)
	if err != nil {
		logrus.Infof("couldn't connect to %s: %v", conn.BrokerAddress, err)
		conn.LocalConnection.Close()
		return
	}
	if tcpConn, ok := server.(*net.TCPConn); ok {
		if err := c.tcpConnOptions.setTCPConnOptions(tcpConn); err != nil {
			logrus.Infof("WARNING: Error while setting TCP options for kafka connection %s on %v: %v", conn.BrokerAddress, server.LocalAddr(), err)
		}
	}
	c.conns.Add(conn.BrokerAddress, conn.LocalConnection)
	copyThenClose(c.processorConfig, server, conn.LocalConnection, conn.BrokerAddress, conn.BrokerAddress, "local connection on "+conn.LocalConnection.LocalAddr().String())
	if err := c.conns.Remove(conn.BrokerAddress, conn.LocalConnection); err != nil {
		logrus.Info(err)
	}
}

func (c *Client) DialAndAuth(brokerAddress string, tlsConfig *tls.Config) (net.Conn, error) {
	conn, err := c.dial(brokerAddress, tlsConfig)
	if err != nil {
		return nil, err
	}
	err = c.auth(conn)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *Client) dial(brokerAddress string, tlsConfig *tls.Config) (net.Conn, error) {
	dialer := net.Dialer{
		Timeout:   c.config.Kafka.DialTimeout,
		KeepAlive: c.config.Kafka.KeepAlive,
	}
	if c.config.Kafka.TLS.Enable {
		if tlsConfig == nil {
			return nil, errors.New("tlsConfig must not be nil")
		}
		return tls.DialWithDialer(&dialer, "tcp", brokerAddress, tlsConfig)
	}
	return dialer.Dial("tcp", brokerAddress)
}

func (c *Client) auth(conn net.Conn) error {
	if c.config.Auth.Gateway.Client.Enable {
		authClient := AuthClient{
			conn:    conn,
			magic:   c.config.Auth.Gateway.Client.Magic,
			method:  c.config.Auth.Gateway.Client.Method,
			timeout: c.config.Auth.Gateway.Client.Timeout,
		}
		if err := authClient.sendAndReceiveAuth(); err != nil {
			conn.Close()
			return err
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			return err
		}
	}
	if c.config.Kafka.SASL.Enable {
		saslPlainAuth := SASLPlainAuth{
			conn:         conn,
			clientID:     c.config.Kafka.ClientID,
			writeTimeout: c.config.Kafka.WriteTimeout,
			readTimeout:  c.config.Kafka.ReadTimeout,
			username:     c.config.Kafka.SASL.Username,
			password:     c.config.Kafka.SASL.Password,
		}
		err := saslPlainAuth.sendAndReceiveSASLPlainAuth()
		if err != nil {
			conn.Close()
			return err
		}
		// reset deadlines
		return conn.SetDeadline(time.Time{})
	}
	return nil
}
