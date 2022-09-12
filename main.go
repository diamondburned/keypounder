package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/diamondburned/go-buttplug"
	"github.com/diamondburned/go-buttplug/device"
	"github.com/diamondburned/go-buttplug/intiface"
	"github.com/diamondburned/keypounder/internal/lazytime"
	"github.com/pkg/errors"
	"github.com/viamrobotics/evdev"
)

var (
	intifacePort = 20000
	intifaceName = "intiface-cli"
	useAllMotors = false
)

func init() {
	flag.StringVar(&intifaceName, "intiface-name", intifaceName, "arg0 for intiface CLI")
	flag.IntVar(&intifacePort, "intiface-port", intifacePort, "port for intiface CLI")
	flag.BoolVar(&useAllMotors, "use-all-motors", useAllMotors, "use all motors instead of 1")
}

func main() {
	vibrations.Key.bindFlags("key")
	vibrations.RelativeVibration.bindFlags("mouse")
	vibrations.AbsoluteVibration.bindFlags("tablet")

	flag.Parse()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	ws := intiface.NewWebsocket(intifacePort, intifaceName)
	dm := device.NewManager()
	defer dm.Wait()

	// Give the buttplug instance a context that we cancel later, because we
	// don't want SIGINT to stop the buttplug event loop until we stopped
	// vibrating. Note how we run cancel before waiting.
	butt := dm.ListenPassthrough(ws.Open(ctx))
	evch := make(chan *evdev.EventEnvelope)

	var wg sync.WaitGroup
	defer wg.Wait()

	if err := listenAllInputs(ctx, evch, &wg); err != nil {
		log.Fatalln("cannot listen to any inputs:", err)
	}

	{
		f, err := os.Create("/tmp/keypounder.pprof")
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close() // error handling omitted for example
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	start(ctx, ws, dm, butt, evch)
	stop()
}

func start(
	ctx context.Context,
	ws *intiface.Websocket, dm *device.Manager,
	butt <-chan buttplug.Message, evch <-chan *evdev.EventEnvelope,
) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	vmanager := newVibratorManager()
	defer vmanager.mustStop()

	var timeout lazytime.Timer
	defer timeout.Stop()

	keepAlive := time.NewTicker(time.Minute)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-butt:
			switch ev := ev.(type) {
			case error:
				log.Println("buttplug error:", ev)
			case *buttplug.ServerInfo:
				ws.Send(ctx,
					&buttplug.StartScanning{},
					&buttplug.RequestDeviceList{},
				)
			case *buttplug.DeviceAdded, *buttplug.DeviceRemoved, *buttplug.DeviceList:
				log.Println("refreshing vibrator list")
				vmanager.refresh(dm, ws)
				log.Printf("now controlling %d vibrators", len(vmanager.vibrators))
			}
		case ev := <-evch:
			if data := eventVibration(ev.Event.Type); data.strength > 0 {
				vmanager.vibrate(data.strength, data.duration, eventTime(ev))
				timeout.Reset(data.duration)
			}
		case <-timeout.C:
			vmanager.stopVibrating()
		case <-keepAlive.C:
			vmanager.keepVibrating()
		}
	}
}

var vibrations = struct {
	Key               vibrationData
	RelativeVibration vibrationData
	AbsoluteVibration vibrationData
}{
	Key:               vibrationData{1.0, 75 * time.Millisecond},
	RelativeVibration: vibrationData{0.4, 50 * time.Millisecond},
	AbsoluteVibration: vibrationData{0.4, 50 * time.Millisecond},
}

func eventVibration(evt evdev.EventType) vibrationData {
	switch evt {
	case evdev.EventKey:
		return vibrations.Key
	case evdev.EventRelative:
		return vibrations.RelativeVibration
	case evdev.EventAbsolute:
		return vibrations.AbsoluteVibration
	default:
		return vibrationData{}
	}
}

type vibrationData struct {
	strength float64
	duration time.Duration // min 50ms
}

func (v *vibrationData) bindFlags(name string) {
	flag.Float64Var(&v.strength, name+"-strength", v.strength,
		"vibration strength for "+name+" [0.0, 1.0]")
	flag.DurationVar(&v.duration, name+"-duration", v.duration,
		"vibration duration for "+name)
}

type vibratorManager struct {
	vibrators []*device.Controller
	strength  float64

	// reduceAt keeps the time to start allowing the vibrator's strengths to be
	// reduced from a higher level. It is used to prioritize stronger
	// vibrations.
	reduceAt time.Time
}

func newVibratorManager() *vibratorManager {
	return &vibratorManager{}
}

func (m *vibratorManager) refresh(deviceMan *device.Manager, conn device.ButtplugConnection) {
	devices := deviceMan.Devices()
	m.vibrators = make([]*device.Controller, 0, len(devices))

	for _, device := range devices {
		controller := deviceMan.Controller(conn, device.Index)
		if controller.VibrationMotors() > 0 {
			controller = controller.WithAsync()
			m.vibrators = append(m.vibrators, controller)
		}
	}
}

func (m *vibratorManager) stopVibrating() {
	m.vibrate(0, 0, time.Time{})
}

func (m *vibratorManager) keepVibrating() {
	m.vibrate(m.strength, 0, time.Time{})
}

func (m *vibratorManager) vibrate(strength float64, d time.Duration, t time.Time) {
	if d > 0 && strength != 0 {
		// Only return if the higher strength's time hasn't expired yet.
		if m.strength > strength && m.reduceAt.After(t) {
			return
		}
		// Update reduceAt if the new strength is stronger.
		if strength >= m.strength {
			m.reduceAt = t.Add(d)
		}
	}

	if m.strength == strength {
		return
	}

	m.strength = strength

	for _, vibrator := range m.vibrators {
		setAllMotors(vibrator, strength)
	}
}

func (m *vibratorManager) mustStop() {
	m.strength = 0
	// TODO: use event looping to determine that all vibration commands have
	// arrived. That's probably not possible, though. We're also waiting out on
	// the debouncing here.
	time.Sleep(200 * time.Millisecond)
	for _, vibrator := range m.vibrators {
		setAllMotors(vibrator.WithoutAsync(), 0)
	}
}

func setAllMotors(d *device.Controller, strength float64) {
	var m map[int]float64

	if useAllMotors {
		n := d.VibrationMotors()
		m = make(map[int]float64, n)
		for i := 0; i < n; i++ {
			m[i] = strength
		}
	} else {
		m = map[int]float64{0: strength}
	}

	d.Vibrate(m)
}

var supportedEvents = [0x1F]bool{
	evdev.EventKey:      true,
	evdev.EventRelative: true,
	evdev.EventAbsolute: true,
}

func eventTime(ev *evdev.EventEnvelope) time.Time {
	return time.Unix(ev.Time.Unix())
}

func listenAllInputs(ctx context.Context, dst chan<- *evdev.EventEnvelope, wg *sync.WaitGroup) error {
	d, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return errors.Wrap(err, "cannot glob for /dev/input/events*")
	}

	var fail int
	for _, f := range d {
		if err := listenInput(ctx, dst, wg, f); err != nil {
			log.Printf("device error: %s", err)
			fail++
		}
	}

	if fail == 0 {
		return nil
	}

	return fmt.Errorf("failed to start %d devices", fail)
}

func listenInput(ctx context.Context, dst chan<- *evdev.EventEnvelope, wg *sync.WaitGroup, path string) error {
	d, err := openInput(path)
	if err != nil || d == nil {
		return err
	}

	wg.Add(2)
	log.Println("listening to", path)

	go func() {
		<-ctx.Done()
		d.Close()
		wg.Done()
	}()

	go func() {
		defer wg.Done()
		defer d.Close()

		for ev := range d.Poll(ctx) {
			if !supportedEvents[ev.Event.Type] {
				continue
			}
			// Ignore KeyUp: down is 1, up is 0, hold is 2.
			if ev.Event.Type == evdev.EventKey && ev.Value == 0 {
				continue
			}

			select {
			case dst <- ev:
				// ok
			case <-ctx.Done():
				return
			}
		}

		log.Println("device", path, "stopped listening")
	}()

	return nil
}

// openInput opens the given evdev device and checks that it's what we want. It
// does not close the device.
func openInput(path string) (*evdev.Evdev, error) {
	d, err := evdev.OpenFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open evdev device")
	}

	var supported bool

	for et, ok := range d.EventTypes() {
		if !ok || !supportedEvents[et] {
			continue
		}
		supported = true
	}

	if !supported {
		d.Close()
		return nil, nil
	}

	return d, nil
}
