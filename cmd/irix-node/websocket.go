// WebSocket 服务端最小实现（RFC 6455），纯标准库。
//
// 仅实现实时控制台（/api/instance/console/ws）所需的文本帧与
// ping/pong/close 控制帧；数据帧不支持分片与二进制帧（对端为自研客户端）。
// 服务端发送不掩码，客户端帧必须掩码（协议要求，未掩码按协议错误断开）。

package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// wsGUID RFC 6455 握手魔数（Sec-WebSocket-Accept 计算用）。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxFrame 单帧载荷上限（防恶意超大帧内存放大）。
const wsMaxFrame = 1 << 20 // 1 MiB

// WebSocket 帧 opcode。
const (
	wsText   = 0x1
	wsBinary = 0x2
	wsClose  = 0x8
	wsPing   = 0x9
	wsPong   = 0xA
)

// wsConn WebSocket 连接（服务端视角）。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	wmu  sync.Mutex // 写锁：读循环（pong/close/命令回执）与写循环（输出行）并发发送
}

// wsAccept 计算 Sec-WebSocket-Accept。
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// upgradeWS 完成 WebSocket 握手并接管底层连接。
// 失败时已通过 w 输出错误响应（HTTP 层仍可用）。
func upgradeWS(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "缺少 Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("缺少 Sec-WebSocket-Key")
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "非 WebSocket 升级请求", http.StatusBadRequest)
		return nil, errors.New("非 WebSocket 升级请求")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "当前服务器不支持 WebSocket", http.StatusInternalServerError)
		return nil, errors.New("底层 ResponseWriter 不支持 Hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "WebSocket 升级失败: "+err.Error(), http.StatusInternalServerError)
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: brw.Reader, bw: brw.Writer}, nil
}

// readFrame 读取一帧，返回 opcode 与载荷（已去掩码）。
// 客户端帧未掩码、载荷超限、二进制帧与分片帧均按协议错误返回。
func (c *wsConn) readFrame() (int, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	fin := hdr[0]&0x80 != 0
	opcode := int(hdr[0] & 0x0f)
	masked := hdr[1]&0x80 != 0
	length := int64(hdr[1] & 0x7f)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(b[:]))
	} else if length == 127 {
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(b[:]))
	}
	if length > wsMaxFrame {
		return 0, nil, fmt.Errorf("帧过大: %d 字节", length)
	}
	if !masked {
		return 0, nil, errors.New("客户端帧未掩码")
	}
	var key [4]byte
	if _, err := io.ReadFull(c.br, key[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	switch {
	case opcode == wsBinary:
		return 0, nil, errors.New("不支持二进制帧")
	case !fin && opcode < wsClose:
		return 0, nil, errors.New("不支持分片数据帧")
	}
	return opcode, payload, nil
}

// writeFrame 发送一帧（服务端不掩码）。
func (c *wsConn) writeFrame(opcode int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := len(payload)
	hdr := make([]byte, 10)
	hdr[0] = 0x80 | byte(opcode) // FIN + opcode
	switch {
	case n < 126:
		hdr[1] = byte(n)
		if _, err := c.bw.Write(hdr[:2]); err != nil {
			return err
		}
	case n <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		if _, err := c.bw.Write(hdr[:4]); err != nil {
			return err
		}
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		if _, err := c.bw.Write(hdr[:10]); err != nil {
			return err
		}
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// writeText 发送文本帧。
func (c *wsConn) writeText(s string) error {
	return c.writeFrame(wsText, []byte(s))
}

// writePong 回复 pong 帧（回显载荷）。
func (c *wsConn) writePong(payload []byte) error {
	return c.writeFrame(wsPong, payload)
}

// writeClose 发送 close 帧。
func (c *wsConn) writeClose(code uint16, reason string) error {
	p := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(p, code)
	copy(p[2:], reason)
	return c.writeFrame(wsClose, p)
}

// Close 关闭底层 TCP 连接。
func (c *wsConn) Close() error {
	return c.conn.Close()
}
