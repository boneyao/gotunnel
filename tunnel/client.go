//
//   date  : 2014-07-16
//   author: xjdrew
//

package tunnel

import (
	"container/heap"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type Client struct {
	app  *App
	cq   HubQueue
	lock sync.Mutex
	wg   sync.WaitGroup
}

func (cli *Client) createHub() (hub *HubItem, err error) {
	conn, err := net.DialTCP("tcp", nil, cli.app.baddr)
	if err != nil {
		return
	}
	Info("create tunnel: %v <-> %v", conn.LocalAddr(), conn.RemoteAddr())

	// auth
	challenge := make([]byte, TaaBlockSize)
	if _, err = io.ReadFull(conn, challenge); err != nil {
		Error("read challenge failed(%v):%s", conn.RemoteAddr(), err)
		return
	}
	Debug("challenge(%v), len %d, %v", conn.RemoteAddr(), len(challenge), challenge)

	a := NewTaa(cli.app.Secret)
	token, ok := a.ExchangeCipherBlock(challenge)
	if !ok {
		err = errors.New("exchange chanllenge failed")
		Error("exchange challenge failed(%v)", conn.RemoteAddr())
		return
	}

	Debug("token(%v), len %d, %v", conn.RemoteAddr(), len(token), token)
	if _, err = conn.Write(token); err != nil {
		Error("write token failed(%v):%s", conn.RemoteAddr(), err)
		return
	}

	hub = &HubItem{
		Hub: newHub(newTunnel(conn, a.GetRc4key()), true),
	}
	return
}

func (cli *Client) addHub(item *HubItem) {
	cli.lock.Lock()
	heap.Push(&cli.cq, item)
	cli.lock.Unlock()
}

func (cli *Client) removeHub(item *HubItem) {
	cli.lock.Lock()
	heap.Remove(&cli.cq, item.index)
	cli.lock.Unlock()
}

func (cli *Client) fetchHub() *HubItem {
	defer cli.lock.Unlock()
	cli.lock.Lock()

	if len(cli.cq) == 0 {
		return nil
	}
	item := cli.cq[0]
	item.priority += 1
	heap.Fix(&cli.cq, 0)
	return item
}

func (cli *Client) dropHub(item *HubItem) {
	cli.lock.Lock()
	item.priority -= 1
	heap.Fix(&cli.cq, item.index)
	cli.lock.Unlock()
}

func (cli *Client) handleConn(hub *HubItem, conn BiConn) {
	defer conn.Close()
	defer Recover()
	defer cli.dropHub(hub)

	linkid := hub.AcquireId()
	if linkid == 0 {
		Error("alloc linkid failed, source: %v", conn.RemoteAddr())
		return
	}
	defer hub.ReleaseId(linkid)

	Info("link(%d) create link, source: %v", linkid, conn.RemoteAddr())
	link := hub.NewLink(linkid)
	defer hub.ReleaseLink(linkid)

	link.SendCreate()
	link.Pump(conn)
}

func (cli *Client) listen() {
	defer cli.wg.Done()

	ln, err := net.ListenTCP("tcp", cli.app.laddr)
	if err != nil {
		Panic("listen failed:%v", err)
	}

	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			Log("acceept failed:%s", err.Error())
			if opErr, ok := err.(*net.OpError); ok {
				if !opErr.Temporary() {
					break
				}
			}
			continue
		}
		Info("new connection from %v", conn.RemoteAddr())
		hub := cli.fetchHub()
		if hub == nil {
			Error("no active hub")
			conn.Close()
			continue
		}

		conn.SetKeepAlive(true)
		conn.SetKeepAlivePeriod(time.Second * 60)
		go cli.handleConn(hub, conn)
	}
}

func (cli *Client) Start() error {
	sz := cap(cli.cq)
	done := make(chan error, sz)
	for i := 0; i < sz; i++ {
		go func(index int) {
			Recover()

			first := true
			for {
				hub, err := cli.createHub()
				if first {
					first = false
					done <- err
					if err != nil {
						Error("tunnel %d connect failed", index)
						break
					}
				} else if err != nil {
					Error("tunnel %d reconnect failed", index)
					time.Sleep(time.Second * 3)
					continue
				}

				Error("tunnel %d connect succeed", index)
				cli.addHub(hub)
				hub.Start()
				cli.removeHub(hub)
				Error("tunnel %d disconnected", index)
			}
		}(i)
	}

	for i := 0; i < sz; i++ {
		err := <-done
		if err != nil {
			return err
		}
	}

	cli.wg.Add(1)
	go cli.listen()
	return nil
}

func (cli *Client) Wait() {
	cli.wg.Wait()
	Log("tunnel client quit")
}

func (cli *Client) Status() {
	for _, hub := range cli.cq {
		hub.Status()
	}
}

func newClient(app *App) *Client {
	return &Client{
		app: app,
		cq:  make(HubQueue, app.Tunnels)[0:0],
	}
}
