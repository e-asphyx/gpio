package bcm2708

import (
	"github.com/e-asphyx/gpio"
	"golang.org/x/sys/unix"
	"log"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

const (
	bcm2835PeriBase   = 0x20000000
	gpioBase          = bcm2835PeriBase + 0x200000
	fselOffset        = 0  // 0x0000
	setOffset         = 7  // 0x001c / 4
	clrOffset         = 10 // 0x0028 / 4
	pinLevelOffset    = 13 // 0x0034 / 4
	eventDetectOffset = 16 // 0x0040 / 4
	risingEdOffset    = 19 // 0x004c / 4
	fallingEdOffset   = 22 // 0x0058 / 4
	highDetectOffset  = 25 // 0x0064 / 4
	lowDetectOffset   = 28 // 0x0070 / 4
	pullUpDnOffset    = 37 // 0x0094 / 4
	pullUpDnClkOffset = 38 // 0x0098 / 4

	pullUpDnClkDelay = 2 * time.Microsecond
)

type bcm2835Driver struct {
	mapping []byte
	reg     []uint32
	mutex   sync.Mutex
}

type Pin int

type bcm2708Trigger struct {
	pin     *gpio.Pin
	trigger gpio.PinTrigger
}

var drv *bcm2835Driver

func newBcm2835Driver() (drv *bcm2835Driver, err error) {
	fd, err := os.OpenFile("/dev/mem", os.O_RDWR|os.O_SYNC, 0666)
	if err != nil {
		return nil, err
	}

	pageSize := unix.Getpagesize() // 4096 is hardcoded
	mapping, err := unix.Mmap(int(fd.Fd()), gpioBase, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	fd.Close()

	// construct []uint32 by hands
	sh := reflect.SliceHeader{
		Data: uintptr(unsafe.Pointer(&mapping[0])),
		Len:  len(mapping) / 4,
		Cap:  cap(mapping) / 4,
	}

	reg := *(*[]uint32)(unsafe.Pointer(&sh))
	drv = &bcm2835Driver{
		mapping: mapping,
		reg:     reg,
	}
	runtime.SetFinalizer(drv, (*bcm2835Driver).close)

	return drv, nil
}

func (drv *bcm2835Driver) close() error {
	drv.reg = []uint32{}
	return unix.Munmap(drv.mapping)
}

func (pin Pin) Read() (int, error) {
	offset := pinLevelOffset + int(pin)/32
	return int((drv.reg[offset] >> (uint(pin) & 31)) & 1), nil
}

func (pin Pin) Write(value int) error {
	var offset int

	if value != 0 {
		offset = setOffset + int(pin)/32
	} else {
		offset = clrOffset + int(pin)/32
	}

	drv.reg[offset] = 1 << (uint(pin) & 31)
	return nil
}

func (pin Pin) Direction() gpio.Direction {
	offset := fselOffset + int(pin)/10
	shift := (uint(pin) % 10) * 3
	val := (drv.reg[offset] >> shift) & 7
	if val == 0 {
		return gpio.DirIn
	} else {
		return gpio.DirOut
	}
}

func (pin Pin) SetDirection(dir gpio.Direction) {
	offset := fselOffset + int(pin)/10
	shift := (uint(pin) % 10) * 3
	var mode uint32
	if dir == gpio.DirIn {
		mode = 0
	} else {
		mode = 1
	}

	drv.mutex.Lock()
	drv.reg[offset] = (drv.reg[offset] & ^(7 << shift)) | (mode << shift)
	drv.mutex.Unlock()
}

func (pin Pin) SetPullUpDown(pull gpio.Pull) {
	var val uint32
	switch pull {
	case gpio.PullOff:
		val = 0
	case gpio.PullDown:
		val = 1
	default:
		val = 2
	}
	clkOffset := pullUpDnClkOffset + int(pin)/32

	drv.mutex.Lock()

	drv.reg[pullUpDnOffset] = val
	time.Sleep(pullUpDnClkDelay)

	drv.reg[clkOffset] = 1 << (uint(pin) & 31)
	time.Sleep(pullUpDnClkDelay)

	drv.reg[pullUpDnOffset] = 0
	drv.reg[clkOffset] = 0

	drv.mutex.Unlock()
}

func (pin Pin) Trigger(trigger gpio.Trigger) (gpio.PinTrigger, error) {
	gpioPin, err := gpio.NewPin(int(pin))
	if err != nil {
		return nil, err
	}

	gpioEdge, err := gpioPin.Trigger(trigger)
	if err != nil {
		return nil, err
	}

	tr := &bcm2708Trigger{
		pin:     gpioPin,
		trigger: gpioEdge,
	}

	return tr, nil
}

func (pin Pin) TriggerWithDebounce(edge gpio.Trigger, interval time.Duration) (gpio.PinTrigger, error) {
	// use software debounce
	return gpio.NewDebounceWithInterval(pin, edge, interval)
}

func (tr *bcm2708Trigger) Ch() <-chan int {
	return tr.trigger.Ch()
}

func (tr *bcm2708Trigger) Close() error {
	err := tr.trigger.Close()
	if err != nil {
		return err
	}
	return tr.pin.Close()
}

func (tr *bcm2708Trigger) Trigger() gpio.Trigger {
	return tr.trigger.Trigger()
}

func init() {
	var err error
	drv, err = newBcm2835Driver()
	if err != nil {
		log.Fatal(err)
	}
}
