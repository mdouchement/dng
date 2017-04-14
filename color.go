package dng

import "image/color"

// NYCbCrAModel for the standard color types.
var NYCbCrAModel color.Model = color.ModelFunc(nycbcraModel)

func nycbcraModel(c color.Color) color.Color {
	if _, ok := c.(color.NYCbCrA); ok {
		return c
	}
	return c
}
