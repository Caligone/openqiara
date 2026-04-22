package fbxbus

import (
	"context"
	"net"
	"time"
)

// Listen démarre la boucle de réception des signaux push sur la connexion
// principale. Doit être lancée dans une goroutine après Dial + Subscribe.
//
// Les signaux reçus sont publiés sur le channel Signals().
// Listen retourne quand ctx est annulé ou que la connexion est fermée.
//
// Le RE de fbxbusctl `register` montre que les signaux arrivent uniquement
// sur la connexion principale via fbxevent_wait (= read en epoll côté libc).
// Pas besoin de socket p2p annexe.
func (c *Client) Listen(ctx context.Context) error {
	buf := make([]byte, 8192)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, err := c.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if c.closed.Load() {
				return nil
			}
			return err
		}
		c.handleFrame(buf[:n], "main")
	}
}

// handleFrame parse une frame et l'achemine vers le channel signals si c'est
// bien un signal (msg type 3 ou heuristique).
func (c *Client) handleFrame(data []byte, source string) {
	if len(data) < 12 {
		c.logger.Debug("fbxbus: frame too short", "src", source, "len", len(data))
		return
	}
	msg, err := parseMessage(data)
	if err != nil {
		c.logger.Debug("fbxbus: parse error", "src", source, "err", err)
		return
	}
	c.logger.Debug("fbxbus: frame",
		"src", source,
		"type", msg.Type,
		"serial", msg.Serial,
		"reply_to", msg.ReplyTo,
		"path", msg.Path,
		"member", msg.Member,
		"sender", msg.Sender,
		"body_len", len(msg.Body),
	)
	// Type 3 = SIGNAL (DBus convention). Tout autre = ignore.
	if msg.Type == msgSignal || (msg.Type != msgReply && msg.Path != "" && msg.Member != "") {
		sig := Signal{
			Path:   msg.Path,
			Member: msg.Member,
			Sender: msg.Sender,
			Body:   msg.Body,
		}
		select {
		case c.signals <- sig:
		default:
			c.logger.Warn("fbxbus: signal channel full, dropping", "path", msg.Path, "member", msg.Member)
		}
	}
}
