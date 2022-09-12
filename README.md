# keypounder

Program that turns on the vibrator every key press/mouse move. Uses the
[go-buttplug][go-buttplug] library with the [buttplug.io][buttplug.io] API.

[go-buttplug]: https://github.com/diamondburned/go-buttplug
[buttplug.io]: https://buttplug.io/

## Requirements

The user must be in the `input` group. Running as `root` may work, but Bluez
might not.

`intiface-cli` and the Go compiler are required.

## Building

```sh
go build
```

## Running

```sh
./keypounder
```
