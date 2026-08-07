// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package channelgateway

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"strings"
)

type ImagePolicy struct {
	MaximumInputBytes  int64
	MaximumOutputBytes int64
	MaximumWidth       int
	MaximumHeight      int
}

type ConvertedImage struct {
	Data     []byte
	MIMEType string
	Width    int
	Height   int
	Changed  bool
}

// ConvertImage performs bounded, deterministic server-side conversion for
// channels with stricter media limits. It decodes real JPEG/PNG input, scales
// to the channel dimensions, and lowers JPEG quality until the byte ceiling is
// met. Audio/video are passed only when already within adapter limits; their
// codecs are deliberately adapter-owned rather than guessed here.
func ConvertImage(reader io.Reader, policy ImagePolicy) (ConvertedImage, error) {
	if policy.MaximumInputBytes <= 0 || policy.MaximumOutputBytes <= 0 || policy.MaximumWidth <= 0 || policy.MaximumHeight <= 0 {
		return ConvertedImage{}, errors.New("complete positive image limits are required")
	}
	input, err := io.ReadAll(io.LimitReader(reader, policy.MaximumInputBytes+1))
	if err != nil {
		return ConvertedImage{}, err
	}
	if int64(len(input)) > policy.MaximumInputBytes {
		return ConvertedImage{}, fmt.Errorf("image exceeds the %d-byte input limit", policy.MaximumInputBytes)
	}
	source, format, err := image.Decode(bytes.NewReader(input))
	if err != nil {
		return ConvertedImage{}, errors.New("media is not a supported JPEG or PNG image")
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return ConvertedImage{}, errors.New("image dimensions are invalid")
	}
	scale := math.Min(1, math.Min(float64(policy.MaximumWidth)/float64(width), float64(policy.MaximumHeight)/float64(height)))
	targetWidth := max(1, int(math.Round(float64(width)*scale)))
	targetHeight := max(1, int(math.Round(float64(height)*scale)))
	converted := source
	changed := targetWidth != width || targetHeight != height || strings.ToLower(format) != "jpeg" || int64(len(input)) > policy.MaximumOutputBytes
	if targetWidth != width || targetHeight != height {
		destination := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
		// Area-center nearest sampling is deterministic and does not add a codec
		// dependency to the cloud channel edge.
		for y := 0; y < targetHeight; y++ {
			sy := bounds.Min.Y + min(height-1, int((float64(y)+0.5)*float64(height)/float64(targetHeight)))
			for x := 0; x < targetWidth; x++ {
				sx := bounds.Min.X + min(width-1, int((float64(x)+0.5)*float64(width)/float64(targetWidth)))
				destination.Set(x, y, source.At(sx, sy))
			}
		}
		converted = destination
	}
	var output bytes.Buffer
	for quality := 92; quality >= 42; quality -= 10 {
		output.Reset()
		if err := jpeg.Encode(&output, converted, &jpeg.Options{Quality: quality}); err != nil {
			return ConvertedImage{}, err
		}
		if int64(output.Len()) <= policy.MaximumOutputBytes {
			return ConvertedImage{Data: append([]byte(nil), output.Bytes()...), MIMEType: "image/jpeg", Width: targetWidth, Height: targetHeight, Changed: changed}, nil
		}
	}
	return ConvertedImage{}, fmt.Errorf("image cannot be converted below the %d-byte channel limit", policy.MaximumOutputBytes)
}
