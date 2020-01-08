package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/gorilla/websocket"

	"github.com/gin-gonic/gin"
)

func router() http.Handler {
	r := gin.Default()
	r.Static("ui", "static/noVNC")
	r.GET("", func(c *gin.Context) {
		c.Redirect(http.StatusSeeOther, "/ui/vnc.html")
	})
	r.GET("websockify", websockify)
	return r
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1 << 20,
	WriteBufferSize: 1 << 20,
}

var id int32

func websockify(c *gin.Context) {
	id := atomic.AddInt32(&id, 1)
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

	go func() {
		defer func() {
			cancel()
		}()

		var buf [1 << 20]byte
		for {
			n, err := vncConn.Read(buf[:])
			if err != nil {
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
