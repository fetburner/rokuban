package worker

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// .lgd の形式は logoframe（pin した sha 8185bafc281e86d847d8084de53c7ae42acfb532）の
// src/logo.h に従う: 28 byte のファイルヘッダ文字列、4 byte BigEndian のロゴ数、
// LOGO_HEADER（name[32] + x,y,h,w,fi,fo,st,ed の short 8 個）、続いて h*w 個の
// LOGO_PIXEL（dp_y,y,dp_cb,cb,dp_cr,cr の short 6 個、リトルエンディアン）。
// logo_generator.cpp が書く .lgd はロゴ数 1 で、先頭の 1 件だけをプレビューにする。
const (
	lgdFileHeaderSize = 28 + 4
	lgdLogoHeaderSize = 32 + 8*2
	lgdPixelSize      = 6 * 2
	lgdMaxOpacity     = 1000
	lgdMaxDimension   = 4096
)

// lgdPreviewPNG は .lgd の先頭ロゴを、不透明度をアルファに持つ PNG にする。
// 色は Y/Cb/Cr（12bit 相当）を 8bit に落として JFIF 変換したもので、見た目の確認用。
func lgdPreviewPNG(lgd []byte) ([]byte, error) {
	if len(lgd) < lgdFileHeaderSize+lgdLogoHeaderSize {
		return nil, fmt.Errorf("lgd too short: %d bytes", len(lgd))
	}
	if binary.BigEndian.Uint32(lgd[28:32]) < 1 {
		return nil, fmt.Errorf("lgd has no logo")
	}
	header := lgd[lgdFileHeaderSize:]
	h := int(int16(binary.LittleEndian.Uint16(header[32+4:])))
	w := int(int16(binary.LittleEndian.Uint16(header[32+6:])))
	if h <= 0 || w <= 0 || h > lgdMaxDimension || w > lgdMaxDimension {
		return nil, fmt.Errorf("invalid lgd size %dx%d", w, h)
	}
	pixels := header[lgdLogoHeaderSize:]
	if len(pixels) < h*w*lgdPixelSize {
		return nil, fmt.Errorf("lgd pixel data truncated: %d bytes for %dx%d", len(pixels), w, h)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < h*w; i++ {
		p := pixels[i*lgdPixelSize:]
		v := func(n int) int { return int(int16(binary.LittleEndian.Uint16(p[n*2:]))) }
		y := clamp255(v(1) / 16)
		cb := clamp255(v(3)/16 + 128)
		cr := clamp255(v(5)/16 + 128)
		r, g, b := color.YCbCrToRGB(uint8(y), uint8(cb), uint8(cr))
		alpha := clamp255(v(0) * 255 / lgdMaxOpacity)
		img.SetNRGBA(i%w, i/w, color.NRGBA{R: r, G: g, B: b, A: uint8(alpha)})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encoding preview png: %w", err)
	}
	return buf.Bytes(), nil
}

func clamp255(v int) int { return max(0, min(v, 255)) }
