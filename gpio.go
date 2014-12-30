package gpio

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

type Direction int
type Trigger int
type Pull int

//go:generate stringer -type=Direction
// Pin direction
const (
	DirIn Direction = iota
	DirOut
)

//go:generate stringer -type=Trigger
// Represents signal edge
const (
	EdgeNone Trigger = iota
	EdgeRising
	EdgeFalling
	EdgeBoth
)

//go:generate stringer -type=Pull
// Represents pull up/down resistor mode
const (
	PullOff Pull = iota
	PullDown
	PullUp
)

const DefaultDebounceInterval time.Duration = 5 * time.Millisecond

// Readable pin
type PinReader interface {
	Read() (int, error)
}

// Writable pin
type PinWriter interface {
	Write(value int) error
}

// Readable pin with edge interrupt capabilities
type PinReadTrigger interface {
	PinReader
	Trigger(edge Trigger) (PinTrigger, error)
	TriggerWithDebounce(edge Trigger, interval time.Duration) (PinTrigger, error) //pin can provide hw debounce
}

// Readable/Writable pin
type PinReadWriter interface {
	PinReader
	PinWriter
	Direction() (Direction, error)
}

// Must be closed before making any subsequent Read and Write calls
type PinTrigger interface {
	Ch() <-chan int
	Close() error
	Trigger() Trigger
}

/* ------------------------------------------------------------------------- */

var (
	ErrInvalid = errors.New("Invalid pin")
	ErrTrigger = errors.New("Trigger active")
)

type Pin struct {
	idx     int
	fd      *os.File
	ch      chan int
	trigger Trigger
}

type gpioTrigger Pin //huh

type gpioDebounce struct {
	src PinTrigger
	ch  chan int
}

func openWriteCloseFile(filename, data string) error {
	fd, err := os.OpenFile(filename, os.O_WRONLY, 0666)
	if err != nil {
		return err
	}
	defer fd.Close()
	_, err = fmt.Fprint(fd, data)
	return err
}

func NewPin(num int) (pin *Pin, err error) {
	fileName := fmt.Sprintf("/sys/class/gpio/gpio%d/value", num)

	_, err = os.Stat(fileName)
	if err != nil {
		err = openWriteCloseFile("/sys/class/gpio/export", strconv.FormatUint(uint64(num), 10))
		if err != nil {
			return nil, err
		}
	}

	var fd *os.File
	cnt := 0
	for {
		fd, err = os.OpenFile(fileName, os.O_RDWR|os.O_SYNC, 0666)
		if err == nil {
			break
		} else if !os.IsPermission(err) || cnt == 10 {
			return nil, err
		}

		// Wait for permission change by udev
		cnt++
		time.Sleep(10 * time.Millisecond)
	}

	pin = &Pin{idx: num, fd: fd}
	runtime.SetFinalizer(pin, (*Pin).Close)

	return pin, nil
}

func (pin *Pin) Read() (int, error) {
	if pin.ch != nil {
		return 0, ErrTrigger
	}

	val, err := pin.read()
	if err != nil {
		return 0, err
	}
	return val, nil
}

func (pin *Pin) read() (int, error) {
	_, err := pin.fd.Seek(0, os.SEEK_SET)
	if err != nil {
		return 0, err
	}

	var buf [1]byte
	_, err = pin.fd.Read(buf[:])
	if err != nil {
		return 0, err
	}
	var val int
	if buf[0] == '1' {
		val = 1
	} else {
		val = 0
	}

	return val, nil
}

func (pin *Pin) Write(value int) error {
	if pin.ch != nil {
		return ErrTrigger
	}

	var buf [1]byte
	if value != 0 {
		buf[0] = '1'
	} else {
		buf[0] = '0'
	}
	_, err := pin.fd.Write(buf[:])
	if err != nil {
		return err
	}
	return nil
}

func (pin *Pin) Close() error {
	if pin.ch != nil {
		err := (*gpioTrigger)(pin).Close()
		if err != nil {
			return err
		}
	}

	err := pin.fd.Close()
	if err != nil {
		return err
	}

	return openWriteCloseFile("/sys/class/gpio/unexport", strconv.FormatUint(uint64(pin.idx), 10))
}

func (pin *Pin) Direction() (Direction, error) {
	fd, err := os.Open(fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin.idx))
	if err != nil {
		return DirIn, err
	}
	defer fd.Close()

	var val string
	_, err = fmt.Fscanln(fd, &val)
	if err != nil {
		return DirIn, err
	}

	var v Direction
	if val == "in" {
		v = DirIn
	} else {
		v = DirOut
	}

	return v, nil
}

func (pin *Pin) SetDirection(dir Direction) error {
	if pin.ch != nil {
		return ErrTrigger
	}

	var dirStr string

	if dir == DirIn {
		dirStr = "in"
	} else {
		dirStr = "out"
	}

	return openWriteCloseFile(fmt.Sprintf("/sys/class/gpio/gpio%d/direction", pin.idx), dirStr)
}

func (pin *Pin) setEdge(edge Trigger) error {
	var edgeStr string

	switch edge {
	case EdgeNone:
		edgeStr = "none"

	case EdgeRising:
		edgeStr = "rising"

	case EdgeFalling:
		edgeStr = "falling"

	default:
		edgeStr = "both"
	}

	return openWriteCloseFile(fmt.Sprintf("/sys/class/gpio/gpio%d/edge", pin.idx), edgeStr)
}

func (pin *Pin) Trigger(edge Trigger) (trigger PinTrigger, err error) {
	if pin.ch != nil {
		return (*gpioTrigger)(pin), nil
	}

	err = pin.SetDirection(DirIn)
	if err != nil {
		return nil, err
	}

	err = pin.setEdge(edge)
	if err != nil {
		return nil, err
	}

	pin.trigger = edge
	pin.ch = make(chan int, 64)

	err = epollSrv.addPin(pin)
	if err != nil {
		return nil, err
	}

	return (*gpioTrigger)(pin), nil
}

func (pin *Pin) TriggerWithDebounce(edge Trigger, interval time.Duration) (PinTrigger, error) {
	return NewDebounceWithInterval(pin, edge, interval)
}

func (pin *gpioTrigger) Close() error {
	if pin.ch == nil || pin.fd.Fd() == ^uintptr(0) {
		return ErrInvalid
	}

	err := epollSrv.deletePin((*Pin)(pin))
	if err != nil {
		return err
	}

	// sync
	for range pin.ch {
	}
	pin.ch = nil

	return (*Pin)(pin).setEdge(EdgeNone)
}

func (pin *gpioTrigger) Ch() <-chan int {
	return pin.ch
}

func (pin *gpioTrigger) Trigger() Trigger {
	return pin.trigger
}

func NewDebounceWithInterval(pin PinReadTrigger, trigger Trigger, interval time.Duration) (PinTrigger, error) {
	if interval < 0 {
		interval = DefaultDebounceInterval
	}

	value, err := pin.Read()
	if err != nil {
		return nil, err
	}

	tr, err := pin.Trigger(trigger)
	if err != nil {
		return nil, err
	}

	out := make(chan int)

	go func() {
		timer := time.NewTimer(interval)
		var debounce bool = false

		for {
			select {
			case <-timer.C:
				debounce = false

			case val, ok := <-tr.Ch():
				if !ok {
					close(out)
					return
				}

				if (trigger == EdgeRising && val == 1) ||
					(trigger == EdgeFalling && val == 0) ||
					val != value {

					value = val

					if !debounce {
						out <- val
						debounce = true
						timer.Reset(interval)
					}
				}
			}
		}
	}()

	d := &gpioDebounce{
		src: tr,
		ch:  out,
	}

	return d, nil
}

func NewDebounce(pin PinReadTrigger, trigger Trigger) (PinTrigger, error) {
	return NewDebounceWithInterval(pin, trigger, DefaultDebounceInterval)
}

func (d *gpioDebounce) Close() error {
	err := d.src.Close()
	if err != nil {
		return err
	}

	for range d.ch {
	}

	return nil
}

func (d *gpioDebounce) Ch() <-chan int {
	return d.ch
}

func (d *gpioDebounce) Trigger() Trigger {
	return d.src.Trigger()
}
