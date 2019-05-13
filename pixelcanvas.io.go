/*  D3pixelbot - Custom client, recorder and bot for pixel drawing games
    Copyright (C) 2019  David Vogel

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU General Public License as published by
    the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU General Public License for more details.

    You should have received a copy of the GNU General Public License
    along with this program.  If not, see <https://www.gnu.org/licenses/>.  */

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"log"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var pixelcanvasioChunkSize = pixelSize{64, 64}
var pixelcanvasioChunkCollectionSize = chunkSize{15, 15} // Arraysize of chunks that's returned on the bigchunk request

var pixelcanvasioPalette = []color.Color{
	color.RGBA{255, 255, 255, 255},
	color.RGBA{228, 228, 228, 255},
	color.RGBA{136, 136, 136, 255},
	color.RGBA{34, 34, 34, 255},
	color.RGBA{255, 167, 209, 255},
	color.RGBA{229, 0, 0, 255},
	color.RGBA{229, 149, 0, 255},
	color.RGBA{160, 106, 66, 255},
	color.RGBA{229, 217, 0, 255},
	color.RGBA{148, 224, 68, 255},
	color.RGBA{2, 190, 1, 255},
	color.RGBA{0, 211, 221, 255},
	color.RGBA{0, 131, 199, 255},
	color.RGBA{0, 0, 234, 255},
	color.RGBA{207, 110, 228, 255},
	color.RGBA{130, 0, 128, 255},
}

type connectionPixelcanvasio struct {
	Fingerprint      string
	OnlinePlayers    uint32 // Must be read atomically
	Center           image.Point
	AuthName, AuthID string
	NextPixel        time.Time

	Canvas *canvas

	GoroutineQuit chan struct{} // Closing this channel stops the goroutines
	QuitWaitgroup sync.WaitGroup
	// TODO: Rect channel that receives download requests
}

func newPixelcanvasio(createCanvas bool) (*connectionPixelcanvasio, error) {
	con := &connectionPixelcanvasio{
		Fingerprint:   "11111111111111111111111111111111",
		GoroutineQuit: make(chan struct{}),
	}

	if createCanvas {
		con.Canvas = newCanvas(pixelcanvasioChunkSize, pixelcanvasioPalette)
	}

	// Main goroutine that handles queries and timed things
	con.QuitWaitgroup.Add(1)
	go func() {
		defer con.QuitWaitgroup.Done()

		queryTicker := time.NewTicker(10 * time.Second)
		defer queryTicker.Stop()

		getOnlinePlayers := func() {
			response := &struct {
				Online int `json:"online"`
			}{}
			if err := getJSON("https://pixelcanvas.io/api/online", response); err == nil {
				atomic.StoreUint32(&con.OnlinePlayers, uint32(response.Online))
				log.Printf("Player amount: %v", response.Online)
			}
		}
		getOnlinePlayers()

		for {
			select {
			case <-queryTicker.C:
				getOnlinePlayers()
			// TODO: Handle incoming download requests here
			case <-con.GoroutineQuit:
				return
			}
		}
	}()

	// Main goroutine that handles the websocket connection (It will always try to reconnect)
	con.QuitWaitgroup.Add(1)
	go func() {
		defer con.QuitWaitgroup.Done()

		waitTime := 0 * time.Second
		for {
			select {
			case <-con.GoroutineQuit:
				return
			case <-time.After(waitTime):
			}

			// Any following connection attempt should be delayed a few seconds
			waitTime = 5 * time.Second

			// Get websocket URL
			u, err := con.getWebsocketURL()
			if err != nil {
				log.Printf("Failed to connect to websocket server: %v", err)
				continue
			}

			u.RawQuery = "fingerprint=" + con.Fingerprint

			// Connect to websocket server
			c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil {
				log.Printf("Failed to connect to websocket server %v: %v", u.String(), err)
				continue
			}

			// Wait for and handle external close events, or connection errors.
			quitChannel := make(chan struct{})
			go func(c *websocket.Conn, quitChannel chan struct{}) {
				select {
				case <-con.GoroutineQuit:
					c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					select {
					case <-quitChannel:
					case <-time.After(time.Second):
					}
				case <-quitChannel:
				}
				c.Close()
			}(c, quitChannel)

			// Handle events
			for {
				_, message, err := c.ReadMessage()
				if err != nil {
					log.Printf("Websocket connection error: %v", err)
					break
				}
				if len(message) >= 1 {
					opcode := uint8(message[0])
					switch opcode {
					case 0xC1:
						if len(message) == 7 {
							cx := int16(binary.BigEndian.Uint16(message[1:]))
							cy := int16(binary.BigEndian.Uint16(message[3:]))
							mixed := binary.BigEndian.Uint16(message[5:])
							colorIndex := int(mixed & 0x0F)
							ox := int((mixed >> 4) & 0x3F)
							oy := int((mixed >> 10) & 0x3F)
							log.Printf("Pixelchange: color %v @ chunk %v, %v with offset %v, %v", colorIndex, cx, cy, ox, oy)
							// TODO: Forward events to canvas
							// TODO: Forward invalidate all on connection loss
						}
					default:
						log.Printf("Unknown websocket opcode: %v", opcode)
					}

				}
			}
			close(quitChannel)

		}
	}()

	//fmt.Print(con.authenticateMe())

	return con, nil
}

func (con *connectionPixelcanvasio) getOnlinePlayers() int {
	return int(atomic.LoadUint32(&con.OnlinePlayers))
}

func (con *connectionPixelcanvasio) getWebsocketURL() (u *url.URL, err error) {
	response := &struct {
		URL string `json:"url"`
	}{}
	if err := getJSON("https://pixelcanvas.io/api/ws", response); err != nil {
		return nil, fmt.Errorf("Couldn't retrieve websocket URL: %v", err)
	}

	u, err = url.Parse(response.URL)
	if err != nil {
		return nil, fmt.Errorf("Retrieved invalid websocket URL: %v", err)
	}

	return u, nil
}

func (con *connectionPixelcanvasio) authenticateMe() error {
	request := struct {
		Fingerprint string `json:"fingerprint"`
	}{
		Fingerprint: con.Fingerprint,
	}

	statusCode, _, body, err := postJSON("https://pixelcanvas.io/api/me", "https://pixelcanvas.io/", request)
	if err != nil {
		return err
	}

	response := &struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Center      []int   `json:"center"`
		WaitSeconds float32 `json:"waitSeconds"`
	}{}
	if err := json.Unmarshal(body, response); err != nil {
		return err
	}

	if statusCode != 200 {
		return fmt.Errorf("Authentication failed with wrong status code: %v (body: %v)", statusCode, string(body))
	}

	if len(response.Center) < 2 {
		return fmt.Errorf("Invalid center given in authentication response")
	}

	con.AuthID = response.ID
	con.AuthName = response.Name
	con.Center.X, con.Center.Y = response.Center[0], response.Center[1]
	con.NextPixel = time.Now().Add(time.Duration(response.WaitSeconds*1000) * time.Millisecond)

	return nil
}

func (con *connectionPixelcanvasio) Close() {
	// Stop goroutines gracefully
	close(con.GoroutineQuit)

	con.QuitWaitgroup.Wait()

	return
}