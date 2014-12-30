package gpio

import (
	"golang.org/x/sys/unix"
	"log"
	"os"
)

type epollServer struct {
	wakeup_r *os.File
	wakeup_w *os.File
	fd       *os.File
	add      chan *Pin
	remove   chan *Pin
}

const maxEvents = 64

var epollSrv *epollServer

func newEpollServer() (srv *epollServer, err error) {
	srv = new(epollServer)

	srv.wakeup_r, srv.wakeup_w, err = os.Pipe()
	if err != nil {
		return nil, err
	}

	fd, err := unix.EpollCreate(1)
	if err != nil {
		return nil, err
	}
	srv.fd = os.NewFile(uintptr(fd), "<epoll>")

	evt := unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(srv.wakeup_r.Fd()),
	}

	err = unix.EpollCtl(int(srv.fd.Fd()), unix.EPOLL_CTL_ADD, int(srv.wakeup_r.Fd()), &evt)
	if err != nil {
		return nil, err
	}

	srv.add = make(chan *Pin, 1)
	srv.remove = make(chan *Pin, 1)

	go srv.serve()
	return srv, nil
}

func (srv *epollServer) addPin(pin *Pin) error {
	var buf [1]byte
	srv.add <- pin
	_, err := srv.wakeup_w.Write(buf[:])
	return err
}

func (srv *epollServer) deletePin(pin *Pin) error {
	var buf [1]byte
	srv.remove <- pin
	_, err := srv.wakeup_w.Write(buf[:])
	return err
}

func (srv *epollServer) serve() {
	pins := make(map[int32]*Pin)
	events := make([]unix.EpollEvent, maxEvents)

	defer srv.fd.Close()
	for {
		nfds, err := unix.EpollWait(int(srv.fd.Fd()), events, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}

			log.Println(err)
			return
		}

		for n := 0; n < nfds; n++ {
			if events[n].Fd == int32(srv.wakeup_r.Fd()) {
				var buf [1]byte
				_, err = srv.wakeup_r.Read(buf[:])
				if err != nil {
					log.Println(err)
					return
				}

				for len(srv.add) != 0 {
					pin := <-srv.add
					fd := pin.fd.Fd()

					if _, ok := pins[int32(fd)]; ok {
						continue
					}

					pins[int32(fd)] = pin

					evt := unix.EpollEvent{
						Events: unix.EPOLLPRI | unix.EPOLLERR,
						Fd:     int32(fd),
					}

					err = unix.EpollCtl(int(srv.fd.Fd()), unix.EPOLL_CTL_ADD, int(fd), &evt)
					if err != nil {
						log.Println(err)
						return
					}
				}

				for len(srv.remove) != 0 {
					p := <-srv.remove
					fd := p.fd.Fd()

					var (
						pin *Pin
						ok  bool
					)
					if pin, ok = pins[int32(fd)]; !ok {
						continue
					}

					err = unix.EpollCtl(int(srv.fd.Fd()), unix.EPOLL_CTL_DEL, int(fd), &unix.EpollEvent{})
					if err != nil {
						return
					}

					delete(pins, int32(fd))
					close(pin.ch)
				}

			} else if pin, ok := pins[events[n].Fd]; ok {
				val, err := pin.read()
				if err != nil {
					log.Println(err)
					return
				}

				if len(pin.ch) != cap(pin.ch) {
					pin.ch <- val
				}
			}
		}
	}
}

func init() {
	var err error
	epollSrv, err = newEpollServer()
	if err != nil {
		log.Fatal(err)
	}
}
