module github.com/diamondburned/keypounder

go 1.17

replace github.com/diamondburned/go-buttplug => ../go-buttplug

require (
	github.com/diamondburned/go-buttplug v0.0.3
	github.com/pkg/errors v0.9.1
	github.com/viamrobotics/evdev v0.1.3
)

require github.com/gorilla/websocket v1.4.2 // indirect
