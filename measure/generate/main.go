package main

import (
	"bufio"
	"fmt"
	"os"
)

const (
	width  = 320
	height = 80
	fps    = 25
	scale  = 6
)

var font = map[byte][7]byte{
	'0': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	'3': {0b11110, 0b00001, 0b00001, 0b01110, 0b00001, 0b00001, 0b11110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b10000, 0b11110, 0b00001, 0b00001, 0b11110},
	'6': {0b01110, 0b10000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00001, 0b01110},
	':': {0, 0b00100, 0b00100, 0, 0b00100, 0b00100, 0},
}

func main() {
	out := bufio.NewWriterSize(os.Stdout, width*height*4)
	pixels := make([]byte, width*height)
	const seconds = 11 * 60
	for frame := 0; frame < seconds*fps; frame++ {
		for i := range pixels {
			pixels[i] = 16
		}
		drawTime(pixels, frame)
		if _, err := fmt.Fprintf(out, "P5\n%d %d\n255\n", width, height); err != nil {
			fatal(err)
		}
		if _, err := out.Write(pixels); err != nil {
			fatal(err)
		}
	}
	if err := out.Flush(); err != nil {
		fatal(err)
	}
}

func drawTime(p []byte, frame int) {
	text := fmt.Sprintf("%05d:%02d", frame/fps, frame%fps)
	x := 8
	for _, c := range text {
		for row, bits := range font[byte(c)] {
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) == 0 {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						set(p, x+col*scale+dx, 16+row*scale+dy)
					}
				}
			}
		}
		x += 38
	}
}

func set(p []byte, x, y int) { p[y*width+x] = 235 }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
