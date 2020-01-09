package main

import (
	"context"
	"encoding/binary"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gin-gonic/gin"
)

func router() http.Handler {
	r := gin.Default()
	r.Static("ui", "static/noVNC")
	r.Static("play", "static/noVNCPlayer")
	r.GET("", func(c *gin.Context) {
		c.Redirect(http.StatusSeeOther, "/ui/vnc.html")
	})
	r.GET("websockify", websockify)
	r.GET("playback/:id", playback)
	return r
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 20,
	WriteBufferSize: 1 << 20,
}

func websockify(c *gin.Context) {
	id := time.Now().Format("2006-15-04-05")
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

	vncConn, err := net.Dial("tcp", "10.10.20.12:5901")
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
		os.MkdirAll("sess", os.ModeDir)
		f, err := os.Create(path.Join("sess", id))
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

	f, err := os.Open(path.Join("sess", id))
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
