package ui

import (
	"image/color"
	"time"

	"gioui.org/unit"
)

// design is the single visual vocabulary for the client. Spacing follows an
// eight-point rhythm with a four-point half step; all component styling is
// composed from these values.
type designTokens struct {
	Canvas, Surface, SurfaceRaised           color.NRGBA
	Text, TextMuted, Accent                  color.NRGBA
	AccentText, Success, Warning             color.NRGBA
	Danger, Focus, Grid, Disabled            color.NRGBA
	Space1, Space2, Space3                   unit.Dp
	Space4, Space6, Space8                   unit.Dp
	RadiusSmall, RadiusMedium                unit.Dp
	Border, FocusBorder                      unit.Dp
	TypeSmall, TypeMeta                      unit.Sp
	WindowWidth, WindowHeight                unit.Dp
	CompactInsetWidth, FooterMinHeight       unit.Dp
	HeaderCompactWidth, NavigationStackWidth unit.Dp
	StripTwoColumnWidth, StripOneColumnWidth unit.Dp
	CaptureDuration, CaptureFrame            time.Duration
}

var design = designTokens{
	Canvas:        color.NRGBA{R: 0x0d, G: 0x12, B: 0x1b, A: 0xff},
	Surface:       color.NRGBA{R: 0x16, G: 0x20, B: 0x2c, A: 0xff},
	SurfaceRaised: color.NRGBA{R: 0x20, G: 0x2d, B: 0x3a, A: 0xff},
	Text:          color.NRGBA{R: 0xe8, G: 0xf0, B: 0xf6, A: 0xff},
	TextMuted:     color.NRGBA{R: 0xa9, G: 0xb8, B: 0xc5, A: 0xff},
	Accent:        color.NRGBA{R: 0xd8, G: 0xad, B: 0x52, A: 0xff},
	AccentText:    color.NRGBA{R: 0x18, G: 0x14, B: 0x0d, A: 0xff},
	Success:       color.NRGBA{R: 0x68, G: 0xd2, B: 0x92, A: 0xff},
	Warning:       color.NRGBA{R: 0xf0, G: 0xc5, B: 0x68, A: 0xff},
	Danger:        color.NRGBA{R: 0xee, G: 0x7c, B: 0x7c, A: 0xff},
	Focus:         color.NRGBA{R: 0xf5, G: 0xdc, B: 0x84, A: 0xff},
	Grid:          color.NRGBA{R: 0x24, G: 0x35, B: 0x45, A: 0xff},
	Disabled:      color.NRGBA{R: 0x5f, G: 0x6d, B: 0x78, A: 0xff},
	Space1:        unit.Dp(4), Space2: unit.Dp(8), Space3: unit.Dp(12),
	Space4: unit.Dp(16), Space6: unit.Dp(24), Space8: unit.Dp(32),
	RadiusSmall: unit.Dp(4), RadiusMedium: unit.Dp(8),
	Border: unit.Dp(1), FocusBorder: unit.Dp(2),
	TypeSmall: unit.Sp(12), TypeMeta: unit.Sp(13),
	WindowWidth: unit.Dp(980), WindowHeight: unit.Dp(720),
	CompactInsetWidth: unit.Dp(600), FooterMinHeight: unit.Dp(480),
	HeaderCompactWidth: unit.Dp(620), NavigationStackWidth: unit.Dp(760),
	StripTwoColumnWidth: unit.Dp(680), StripOneColumnWidth: unit.Dp(360),
	CaptureDuration: 650 * time.Millisecond, CaptureFrame: 90 * time.Millisecond,
}

func packed(c color.NRGBA) uint32 {
	return uint32(c.R)<<24 | uint32(c.G)<<16 | uint32(c.B)<<8 | uint32(c.A)
}
