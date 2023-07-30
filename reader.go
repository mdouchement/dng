// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package dng implements rudimentary support for reading Digital Negative
// (DNG) files.
//
// DNG is a bastardized TIFF file.
// This package is a stripped back version of golang.org/x/image/tiff.
//
// Known limitations:
//
// Because TIFF files and DNG files share the same first few bytes, the image
// package's file type detection will fail to recognize a dng if the tiff
// reader is also imported.

// Resources:
// http://wwwimages.adobe.com/content/dam/Adobe/en/products/photoshop/pdfs/dng_spec_1.4.0.0.pdf
// https://github.com/golang/image/tree/master/tiff
// https://github.com/nf/cr2

package dng

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
)

// A FormatError reports that the input is not a valid TIFF image.
type FormatError string

func (e FormatError) Error() string {
	return fmt.Sprintf("dng: invalid format: %s", string(e))
}

// An UnsupportedError reports that the input uses a valid but
// unimplemented feature.
type UnsupportedError string

func (e UnsupportedError) Error() string {
	return fmt.Sprintf("dng: unsupported feature: %s", string(e))
}

// An InternalError reports that an internal error was encountered.
type InternalError string

func (e InternalError) Error() string {
	return fmt.Sprintf("dng: internal error: %s", string(e))
}

type decoder struct {
	r         io.ReaderAt
	byteOrder binary.ByteOrder
	config    image.Config
	mode      imageMode
	bpp       uint
	features  map[int][]uint
	palette   []color.Color

	buf   []byte
	off   int    // Current offset in buf.
	v     uint32 // Buffer value for reading with arbitrary bit depths.
	nbits uint   // Remaining number of bits in v.
}

// firstVal returns the first uint of the features entry with the given tag,
// or 0 if the tag does not exist.
func (d *decoder) firstVal(tag int) uint {
	f := d.features[tag]
	if len(f) == 0 {
		return 0
	}
	return f[0]
}

// ifdUint decodes the IFD entry in p, which must be of the Byte, Short
// or Long type, and returns the decoded uint values.
func (d *decoder) ifdUint(p []byte) (u []uint, err error) {
	var raw []byte
	datatype := d.byteOrder.Uint16(p[2:4])
	count := d.byteOrder.Uint32(p[4:8])
	if datalen := lengths[datatype] * count; datalen > 4 {
		// The IFD contains a pointer to the real value.
		raw = make([]byte, datalen)
		_, err = d.r.ReadAt(raw, int64(d.byteOrder.Uint32(p[8:12])))
	} else {
		raw = p[8 : 8+datalen]
	}
	if err != nil {
		return nil, err
	}

	u = make([]uint, count)
	switch datatype {
	case dtByte:
		for i := uint32(0); i < count; i++ {
			u[i] = uint(raw[i])
		}
	case dtShort:
		for i := uint32(0); i < count; i++ {
			u[i] = uint(d.byteOrder.Uint16(raw[2*i : 2*(i+1)]))
		}
	case dtLong:
		for i := uint32(0); i < count; i++ {
			u[i] = uint(d.byteOrder.Uint32(raw[4*i : 4*(i+1)]))
		}
	default:
		return nil, UnsupportedError("data type")
	}
	return u, nil
}

// parseIFD decides whether the the IFD entry in p is "interesting" and
// stows away the data in the decoder.
func (d *decoder) parseIFD(p []byte) error {
	tag := d.byteOrder.Uint16(p[0:2])
	switch tag {
	case tBitsPerSample,
		tExtraSamples,
		tPhotometricInterpretation,
		tCompression,
		tPredictor,
		tStripOffsets,
		tStripByteCounts,
		tRowsPerStrip,
		tTileWidth,
		tTileLength,
		tTileOffsets,
		tTileByteCounts,
		tImageLength,
		tImageWidth:
		val, err := d.ifdUint(p)
		if err != nil {
			return err
		}
		d.features[int(tag)] = val
	case tColorMap:
		val, err := d.ifdUint(p)
		if err != nil {
			return err
		}
		numcolors := len(val) / 3
		if len(val)%3 != 0 || numcolors <= 0 || numcolors > 256 {
			return FormatError("bad ColorMap length")
		}
		d.palette = make([]color.Color, numcolors)
		for i := 0; i < numcolors; i++ {
			d.palette[i] = color.RGBA64{
				uint16(val[i]),
				uint16(val[i+numcolors]),
				uint16(val[i+2*numcolors]),
				0xffff,
			}
		}
	case tSampleFormat:
		// Page 27 of the spec: If the SampleFormat is present and
		// the value is not 1 [= unsigned integer data], a Baseline
		// TIFF reader that cannot handle the SampleFormat value
		// must terminate the import process gracefully.
		val, err := d.ifdUint(p)
		if err != nil {
			return err
		}
		for _, v := range val {
			if v != 1 {
				return UnsupportedError("sample format")
			}
		}
	}
	return nil
}

// readBits reads n bits from the internal buffer starting at the current offset.
func (d *decoder) readBits(n uint) uint32 {
	for d.nbits < n {
		d.v <<= 8
		d.v |= uint32(d.buf[d.off])
		d.off++
		d.nbits += 8
	}
	d.nbits -= n
	rv := d.v >> d.nbits
	d.v &^= rv << d.nbits
	return rv
}

// flushBits discards the unread bits in the buffer used by readBits.
// It is used at the end of a line.
func (d *decoder) flushBits() {
	d.v = 0
	d.nbits = 0
}

// minInt returns the smaller of x or y.
func minInt(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func newDecoder(r io.Reader) (*decoder, error) {
	d := &decoder{
		r:        newReaderAt(r),
		features: make(map[int][]uint),
	}

	p := make([]byte, 8)
	if _, err := d.r.ReadAt(p, 0); err != nil {
		return nil, err
	}
	switch string(p[0:4]) {
	case leHeader:
		d.byteOrder = binary.LittleEndian
	case beHeader:
		d.byteOrder = binary.BigEndian
	default:
		return nil, FormatError("malformed header")
	}

	ifdOffset := int64(d.byteOrder.Uint32(p[4:8]))

	// The first two bytes contain the number of entries (12 bytes each).
	if _, err := d.r.ReadAt(p[0:2], ifdOffset); err != nil {
		return nil, err
	}
	numItems := int(d.byteOrder.Uint16(p[0:2]))

	// All IFD entries are read in one chunk.
	p = make([]byte, ifdLen*numItems)
	if _, err := d.r.ReadAt(p, ifdOffset+2); err != nil {
		return nil, err
	}

	for i := 0; i < len(p); i += ifdLen {
		if err := d.parseIFD(p[i : i+ifdLen]); err != nil {
			return nil, err
		}
	}

	d.config.Width = int(d.firstVal(tImageWidth))
	d.config.Height = int(d.firstVal(tImageLength))

	if _, ok := d.features[tBitsPerSample]; !ok {
		return nil, FormatError("BitsPerSample tag missing")
	}
	d.bpp = d.firstVal(tBitsPerSample)

	// Determine the image mode.
	switch d.firstVal(tPhotometricInterpretation) {
	case pRGB:
		for _, b := range d.features[tBitsPerSample] {
			if b != 8 {
				return nil, UnsupportedError("non-8-bit RGB image")
			}
		}
		d.config.ColorModel = color.RGBAModel
		// RGB images normally have 3 samples per pixel.
		// If there are more, ExtraSamples (p. 31-32 of the spec)
		// gives their meaning (usually an alpha channel).
		//
		// This implementation does not support extra samples
		// of an unspecified type.
		switch len(d.features[tBitsPerSample]) {
		case 3:
			d.mode = mRGB
		case 4:
			switch d.firstVal(tExtraSamples) {
			case 1:
				d.mode = mRGBA
			case 2:
				d.mode = mNRGBA
				d.config.ColorModel = color.NRGBAModel
			default:
				return nil, FormatError("wrong number of samples for RGB")
			}
		default:
			return nil, FormatError("wrong number of samples for RGB")
		}
	case pPaletted:
		d.mode = mPaletted
		d.config.ColorModel = color.Palette(d.palette)
	case pWhiteIsZero:
		d.mode = mGrayInvert
		if d.bpp == 16 {
			d.config.ColorModel = color.Gray16Model
		} else {
			d.config.ColorModel = color.GrayModel
		}
	case pBlackIsZero:
		d.mode = mGray
		if d.bpp == 16 {
			d.config.ColorModel = color.Gray16Model
		} else {
			d.config.ColorModel = color.GrayModel
		}
	case pYCbCr:
		d.mode = mNYCbCrA
		d.config.ColorModel = color.NYCbCrAModel
	default:
		return nil, UnsupportedError("color model")
	}

	return d, nil
}

// DecodeConfig returns the color model and dimensions of a TIFF image without
// decoding the entire image.
func DecodeConfig(r io.Reader) (image.Config, error) {
	d, err := newDecoder(r)
	if err != nil {
		return image.Config{}, err
	}
	return d.config, nil
}

// NewReader returns an io.Reader to the JPEG thumbnail embedded in the DNG
// image in r.  This allows access to the raw bytes of the JPEG thumbnail
// without the need to decompress it first.
func NewReader(r io.Reader) (io.Reader, error) {
	d, err := newDecoder(r)
	if err != nil {
		return nil, err
	}
	offset := int64(d.features[tStripOffsets][0])
	n := int64(d.features[tStripByteCounts][0])
	switch d.firstVal(tCompression) {
	case cJPEG, cJPEGOld, cLossyJPEG:
	default:
		return nil, UnsupportedError("compression")
	}

	return io.NewSectionReader(d.r, offset, n), nil
}

// Decode reads a DNG image from r and returns the embedded JPEG thumbnail as
// an image.Image.
func Decode(r io.Reader) (image.Image, error) {
	r, err := NewReader(r)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(r)
}

func init() {
	image.RegisterFormat("dng", leHeader, Decode, DecodeConfig)
	image.RegisterFormat("dng", beHeader, Decode, DecodeConfig)
}
