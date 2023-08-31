package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gin-gonic/gin"
)

func router() http.Handler {
	r := gin.Default()
	r.Static("client", "static/noVNC")
	r.Static("player", "static/noVNCPlayer")
	r.Static("rfbplayer", "static/rfbplayer")

	r.GET("/", redirectNoVNC)
	r.GET("websockify/:addr", websockify)
	r.GET("playback/:id", playback)
	r.GET("playback-rfb/:id", playbackRfb)
	r.GET("playback-rfb-dbg", playbackRfbDbg)
	return r
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 20,
	WriteBufferSize: 1 << 20,
}

func redirectNoVNC(c *gin.Context) {
	c.Redirect(http.StatusMovedPermanently, "/client/vnc.html")
}

func websockify(c *gin.Context) {
	addr := c.Param("addr")

	id := time.Now().Unix()
	log.Println("new connection: ", id)
	defer func() {
		log.Println("connection closed: ", id)
	}()
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("upgrade failed: %v\n", err)
		return
	}
	defer conn.Close()

	log.Printf("connect to: %v", addr)
	vncConn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("dial vncserver failed: %v\n", err)
		return
	}
	defer vncConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer func() {
			cancel()
		}()
		for {
			_, r, err := conn.NextReader()
			if err != nil {
				return
			}
			if _, err := io.Copy(vncConn, r); err != nil {
				return
			}
		}
	}()

	serverMsgRecorder := make(chan []byte)
	defer close(serverMsgRecorder)
	go func() {
		defer func() {
			cancel()
		}()

		var buf [1 << 20]byte
		for {
			n, err := vncConn.Read(buf[:])
			if err != nil {
				log.Println("read from vnc conn failed:", err)
				return
			}
			w, err := conn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			_, err = w.Write(buf[:n])
			if err != nil {
				return
			}
			w.Close()

			tmp := make([]byte, n)
			copy(tmp, buf[:n])
			serverMsgRecorder <- tmp
		}
	}()

	go func() {
		defer cancel()
		file := path.Join("resources", "sess", strconv.FormatInt(id, 10))
		os.MkdirAll(filepath.Dir(file), 0755)
		f, err := os.Create(file)
		if err != nil {
			log.Println(err)
		}
		defer f.Close()

		for bytes := range serverMsgRecorder {
			ph := PacketHeader{
				Type:      PacketTypeHandshake,
				Timestamp: time.Now().UnixNano(),
				Length:    int64(len(bytes)),
			}
			if err := binary.Write(f, binary.LittleEndian, ph); err != nil {
				log.Println("write file failed:", err)
				return
			}
			if _, err := f.Write(bytes); err != nil {
				log.Println("write file failed:", err)
				return
			}
		}
	}()
	<-ctx.Done()
}

const (
	PacketTypeHandshake byte = iota
)

type PacketHeader struct {
	Type      byte
	Timestamp int64
	Length    int64
}

func playback(c *gin.Context) {
	id := c.Param("id")
	log.Println("playback", id)
	defer func() {
		log.Println("playback done", id)
	}()

	f, err := os.Open(path.Join("resources", "sess", id))
	if err != nil {
		log.Println("open file failed:", err)
		return
	}
	defer f.Close()

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("upgrade failed: %v\n", err)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		for {
			_, r, err := conn.NextReader()
			if err != nil {
				return
			}
			_, err = ioutil.ReadAll(r)
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer cancel()
		var delta int64
		var init bool
		for {
			var ph PacketHeader
			err := binary.Read(f, binary.LittleEndian, &ph)
			if err != nil {
				if err != io.EOF {
					log.Println("read packet header failed:", err)
				}
				return
			}
			buf := make([]byte, ph.Length)
			if _, err := io.ReadFull(f, buf); err != nil {
				log.Println("read packet failed:", err)
				return
			}

			w, err := conn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			_, err = w.Write(buf[:])
			if err != nil {
				return
			}
			w.Close()

			if !init {
				init = true
				delta = time.Now().UnixNano() - ph.Timestamp
			} else {
				for time.Now().UnixNano() < ph.Timestamp+delta {
					time.Sleep(time.Millisecond)
				}
			}
		}
	}()
	<-ctx.Done()
}

func playbackRfb(c *gin.Context) {
	id := c.Param("id")
	log.Printf("play rfb: %v", id)
	rfbfile := filepath.Clean(filepath.Join("resources", "rfb", id))
	if filepath.Dir(rfbfile) != "resources/rfb" {
		c.AbortWithError(http.StatusForbidden, fmt.Errorf("invalid rfb file: %q", rfbfile))
		return
	}

	wsConn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("upgrade failed: %v\n", err)
		return
	}
	defer wsConn.Close()

	cmd := exec.Command("java", "-cp", "GuiPlayer.jar", "PlayerServer", rfbfile)

	playerServerIn, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("stdin pipe failed: %v", err)
		return
	}
	playerServerOut, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("stdout pipe failed: %v", err)
		return
	}
	playerServerError, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("stderr pipe failed: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer func() {
			cancel()
		}()
		for {
			_, r, err := wsConn.NextReader()
			if err != nil {
				return
			}
			if _, err := io.Copy(playerServerIn, r); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() {
			cancel()
		}()

		var buf [1 << 20]byte
		for {
			n, err := playerServerOut.Read(buf[:])
			if err != nil {
				log.Println("read from out conn failed:", err)
				return
			}
			w, err := wsConn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			_, err = w.Write(buf[:n])
			if err != nil {
				return
			}
			w.Close()
		}
	}()

	go func() {
		defer func() {
			cancel()
		}()

		var buf [1 << 20]byte
		for {
			n, err := playerServerError.Read(buf[:])
			if err != nil {
				log.Println("read from err conn failed:", err)
				return
			}

			log.Printf("err: %s", buf[:n])
		}
	}()

	if err := cmd.Start(); err != nil {
		log.Printf("run guiplayer failed: %v", err)
	}

	<-ctx.Done()
	cmd.Process.Kill()
}

func playbackRfbDbg(c *gin.Context) {
	addr := c.Param("addr")

	if addr == "" {
		addr = "localhost:8889"
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("upgrade failed: %v\n", err)
		return
	}
	defer conn.Close()

	log.Printf("connect to debug server: %v", addr)
	psConn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("dial vncserver failed: %v\n", err)
		return
	}
	defer psConn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer func() {
			cancel()
		}()
		for {
			_, r, err := conn.NextReader()
			if err != nil {
				return
			}
			if _, err := io.Copy(psConn, r); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() {
			cancel()
		}()

		var buf [1 << 20]byte
		for {
			n, err := psConn.Read(buf[:])
			if err != nil {
				log.Println("read from vnc conn failed:", err)
				return
			}
			w, err := conn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			_, err = w.Write(buf[:n])
			if err != nil {
				return
			}
			w.Close()
		}
	}()

	<-ctx.Done()
}
