package fbxbus

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// peerName retourne le nom utilisé par fbxbusctl pour ses sockets internes.
// Format observé : "register-<PID>".
func (c *Client) peerName() string {
	return fmt.Sprintf("register-%d", os.Getpid())
}

// SocketAddr est l'adresse du daemon fbxbus (Linux abstract socket).
// Le prefix "@" est interprété par Go comme byte 0 dans sun_path.
const SocketAddr = "@fbxbus_daemon"

// Signal est un événement push reçu depuis fbxbus.
type Signal struct {
	Path   string // ex: "/fbxhome"
	Member string // ex: "alarm_status_changed"
	Sender string
	Body   []byte // payload brut, à décoder selon la signature attendue
}

// Client est un client fbxbus monothread (un seul Listen actif).
// Les sends sont protégés par mu.
type Client struct {
	conn   *net.UnixConn
	logger *slog.Logger
	myName string // nom assigné par le daemon (ex: ":737-5240")
	serial atomic.Uint32

	signals chan Signal

	mu       sync.Mutex // protège l'écriture sur conn (sends concurrents)
	closed   atomic.Bool
	initOnce sync.Once
}

// Dial ouvre une connexion au bus et effectue le handshake hello.
func Dial(ctx context.Context, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	addr := &net.UnixAddr{Name: SocketAddr, Net: "unixpacket"}
	conn, err := net.DialUnix("unixpacket", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("dial fbxbus: %w", err)
	}
	c := &Client{
		conn:    conn,
		logger:  logger.With("component", "fbxbus"),
		signals: make(chan Signal, 64),
	}
	c.serial.Store(0)

	if err := c.handshake(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	c.logger.Info("fbxbus connected", "name", c.myName)
	return c, nil
}

// Close ferme la connexion. Le listener doit être arrêté avant.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.signals)
	return c.conn.Close()
}

// ensureInitialized exécute la séquence complète d'initialisation observée
// dans fbxbusctl register strace (étapes obligatoires pour que le daemon
// route les signaux vers nous — un simple filter_add ne suffit pas).
//
// Séquence (6 frames C→S + 4 replies + 1 body S→C à drainer) :
//   C→S : filter_add générique
//   C→S : fast "enable"   /fbxbus/p2p/<peerName>
//   C→S : filter_add (re)
//   C→S : fast "enabled"  /fbxbus/monitor/<peerName>
//   C→S : CALL /fbxbus/name request signature="sb"
//   C→S : body : <peerName + NUL>
//
// Doit être appelé AVANT de démarrer Listen (sinon Listen consomme nos replies).
func (c *Client) ensureInitialized() error {
	var err error
	c.initOnce.Do(func() {
		peerName := c.peerName()
		c.logger.Debug("fbxbus init start", "peer", peerName)

		send := func(buf []byte, label string) bool {
			if err != nil {
				return false
			}
			if e := c.send(buf); e != nil {
				err = fmt.Errorf("init %s: %w", label, e)
				return false
			}
			return true
		}

		send(filterAddFrame(c.nextSerial(), c.myName, 0x37), "filter_add#1")
		send(fastFrame(c.nextSerial(), "/fbxbus/p2p/"+peerName, "enable"), "p2p enable")
		send(filterAddFrame(c.nextSerial(), c.myName, 0x37), "filter_add#2")
		send(fastFrame(c.nextSerial(), "/fbxbus/monitor/"+peerName, "enabled"), "monitor enabled")
		send(nameRequestFrame(c.nextSerial(), c.myName), "name_request")
		send(nameRequestBody(peerName), "name_request body")
		if err != nil {
			return
		}

		// Drain les replies du daemon pour ces 4 calls (REPLY_SERIAL).
		// Observé : 4 frames de 48 bytes + 1 body de 4 bytes pour la name request.
		drainStart := time.Now()
		drained := 0
		for time.Since(drainStart) < 3*time.Second && drained < 5 {
			if _, e := c.recv(500 * time.Millisecond); e != nil {
				if ne, ok := e.(net.Error); ok && ne.Timeout() {
					break // plus rien à drainer
				}
				err = fmt.Errorf("init drain: %w", e)
				return
			}
			drained++
		}
		c.logger.Debug("fbxbus init done", "drained", drained)
	})
	return err
}

// Subscribe demande au bus de pousser les signaux (daemon, signalName).
// Doit être appelé après Dial. Peut être appelé plusieurs fois.
func (c *Client) Subscribe(daemon, signalName string) error {
	if c.myName == "" {
		return fmt.Errorf("not authenticated (call Dial first)")
	}
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	body := subscribeBody2(daemon, signalName)
	// body_len dans le header du filter_add doit refléter la taille du body
	// qui suivra (le body est envoyé en SOCK_SEQPACKET séparé après).
	if err := c.send(filterAddFrame(c.nextSerial(), c.myName, uint32(len(body)))); err != nil {
		return fmt.Errorf("filter_add: %w", err)
	}
	if err := c.send(body); err != nil {
		return fmt.Errorf("subscribe body: %w", err)
	}
	c.logger.Info("fbxbus subscribed", "daemon", daemon, "signal", signalName)
	return nil
}

// Signals renvoie le channel de signaux reçus.
func (c *Client) Signals() <-chan Signal {
	return c.signals
}

// Name renvoie le nom assigné par le daemon (vide avant handshake).
func (c *Client) Name() string { return c.myName }

func (c *Client) nextSerial() uint32 {
	return c.serial.Add(1)
}

// send envoie un buffer en respectant l'exclusivité d'écriture.
func (c *Client) send(buf []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Write(buf)
	return err
}

// recv lit un message complet (SEQPACKET → 1 read = 1 message).
// timeout < 0 = bloquant infini.
func (c *Client) recv(timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
		defer c.conn.SetReadDeadline(time.Time{})
	}
	buf := make([]byte, 8192)
	n, err := c.conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// handshake exécute le hello et récupère le nom assigné.
func (c *Client) handshake(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	serial := c.nextSerial()
	if err := c.send(helloFrame(serial)); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Le client envoie son PID en uint32 LE dans un second send séparé.
	// Le daemon l'utilise pour authentifier le client (observé dans le strace).
	pidBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(pidBuf, uint32(os.Getpid()))
	if err := c.send(pidBuf); err != nil {
		return fmt.Errorf("send pid: %w", err)
	}

	// 1er recv : reply header (fields). Le name est envoyé en SEQPACKET séparé.
	reply, err := c.recv(timeout)
	if err != nil {
		return fmt.Errorf("recv hello reply: %w", err)
	}
	msg, err := parseMessage(reply)
	if err != nil {
		return fmt.Errorf("parse hello reply: %w", err)
	}
	if msg.Type != msgReply {
		return fmt.Errorf("hello: expected reply, got type %d", msg.Type)
	}
	if msg.ReplyTo != serial {
		return fmt.Errorf("hello: reply_to=%d, want %d", msg.ReplyTo, serial)
	}

	// 2ᵉ recv : le body avec le name (uint32 len + bytes + NUL).
	nameMsg, err := c.recv(timeout)
	if err != nil {
		return fmt.Errorf("recv hello name: %w", err)
	}
	if len(nameMsg) < 4 {
		return fmt.Errorf("hello: name message too short: %d bytes", len(nameMsg))
	}
	nameLen := binary.LittleEndian.Uint32(nameMsg)
	if 4+int(nameLen) > len(nameMsg) {
		return fmt.Errorf("hello: name length %d overflow message len %d", nameLen, len(nameMsg))
	}
	c.myName = string(nameMsg[4 : 4+nameLen])
	if c.myName == "" {
		return errors.New("hello: server returned empty name")
	}
	return nil
}
