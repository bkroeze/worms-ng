package ui

import (
	"image"
	"image/color"
	"math"

	"gioui.org/f32"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

// Point is a logical odd-row board coordinate. X and Y are zero based.
type Point struct{ X, Y int }

type BoardGeometry struct {
	Width, Height int
	Origin        f32.Point
	DotRadius     float32
	StepX, StepY  float32
}

func NewBoardGeometry(width, height int, bounds image.Point) BoardGeometry {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	maxX := float32(bounds.X) / (float32(width) + 0.5)
	maxY := float32(bounds.Y) / (float32(height)*0.8660254 + 1)
	radius := maxX
	if maxY < radius {
		radius = maxY
	}
	radius *= 0.34
	if radius < 5 {
		radius = 5
	}
	return BoardGeometry{Width: width, Height: height, Origin: f32.Pt(radius*2, radius*2), DotRadius: radius, StepX: radius * 2, StepY: radius * 1.7320508}
}
func (g BoardGeometry) DotAt(p Point) f32.Point {
	return f32.Pt(g.Origin.X+float32(p.X)*g.StepX+float32(p.Y&1)*g.StepX/2, g.Origin.Y+float32(p.Y)*g.StepY)
}
func (g BoardGeometry) InBounds(p Point) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < g.Width && p.Y < g.Height
}

func (g BoardGeometry) ScreenBounds() image.Rectangle {
	if g.Width < 1 || g.Height < 1 {
		return image.Rectangle{}
	}
	minX := g.Origin.X - g.DotRadius
	maxX := g.Origin.X + float32(g.Width-1)*g.StepX + g.StepX/2 + g.DotRadius
	minY := g.Origin.Y - g.DotRadius
	maxY := g.Origin.Y + float32(g.Height-1)*g.StepY + g.DotRadius
	return image.Rect(
		int(math.Floor(float64(minX))),
		int(math.Floor(float64(minY))),
		int(math.Ceil(float64(maxX))),
		int(math.Ceil(float64(maxY))),
	)
}

func (g BoardGeometry) ContainsScreen(p f32.Point) bool {
	bounds := g.ScreenBounds()
	return p.X >= float32(bounds.Min.X) && p.X < float32(bounds.Max.X) &&
		p.Y >= float32(bounds.Min.Y) && p.Y < float32(bounds.Max.Y)
}
func (g BoardGeometry) Territory(p Point) [6]f32.Point {
	c := g.DotAt(p)
	r := g.DotRadius
	var out [6]f32.Point
	for i := range out {
		a := float64(i) * math.Pi / 3
		out[i] = f32.Pt(c.X+r*float32(math.Cos(a)), c.Y+r*float32(math.Sin(a)))
	}
	return out
}

type TrailSegment struct {
	From, To f32.Point
	Color    uint32
}

func EndpointHalfSegments(a, b Point, fromColor, toColor uint32, g BoardGeometry) [2]TrailSegment {
	pa, pb := g.DotAt(a), g.DotAt(b)
	spanX := g.StepX * float32(g.Width)
	spanY := g.StepY * float32(g.Height)
	virtualB := wrappedEndpoint(pa, pb, spanX, spanY)
	virtualA := wrappedEndpoint(pb, pa, spanX, spanY)
	midA := f32.Pt((pa.X+virtualB.X)/2, (pa.Y+virtualB.Y)/2)
	midB := f32.Pt((pb.X+virtualA.X)/2, (pb.Y+virtualA.Y)/2)
	return [2]TrailSegment{{From: pa, To: midA, Color: fromColor}, {From: pb, To: midB, Color: toColor}}
}

func wrappedEndpoint(origin, endpoint f32.Point, spanX, spanY float32) f32.Point {
	dx, dy := endpoint.X-origin.X, endpoint.Y-origin.Y
	if spanX > 0 && float32(math.Abs(float64(dx))) > spanX/2 {
		if dx > 0 {
			endpoint.X -= spanX
		} else {
			endpoint.X += spanX
		}
	}
	if spanY > 0 && float32(math.Abs(float64(dy))) > spanY/2 {
		if dy > 0 {
			endpoint.Y -= spanY
		} else {
			endpoint.Y += spanY
		}
	}
	return endpoint
}
func TrailHalfSegments(trails []Trail, g BoardGeometry) []TrailSegment {
	out := make([]TrailSegment, 0, len(trails)*2)
	for _, t := range trails {
		halves := EndpointHalfSegments(t.A, t.B, t.AColor, t.BColor, g)
		out = append(out, halves[:]...)
	}
	return out
}

type Trail struct {
	A, B           Point
	AColor, BColor uint32
	Owner          string
}

func FillTerritory(ops *op.Ops, g BoardGeometry, p Point, c uint32) {
	vertices := g.Territory(p)
	var path clip.Path
	path.Begin(ops)
	path.MoveTo(vertices[0])
	for _, v := range vertices[1:] {
		path.LineTo(v)
	}
	path.Close()
	paint.FillShape(ops, color.NRGBA{R: byte(c >> 24), G: byte(c >> 16), B: byte(c >> 8), A: byte(c)}, clip.Outline{Path: path.End()}.Op())
}

// OddRowNeighbor returns six neighbors in E, SE, SW, W, NW, NE order.
func OddRowNeighbor(p Point, d Direction) Point {
	switch d {
	case East:
		return Point{X: p.X + 1, Y: p.Y}
	case West:
		return Point{X: p.X - 1, Y: p.Y}
	case SouthEast:
		if p.Y&1 == 0 {
			return Point{X: p.X, Y: p.Y + 1}
		}
		return Point{X: p.X + 1, Y: p.Y + 1}
	case SouthWest:
		if p.Y&1 == 0 {
			return Point{X: p.X - 1, Y: p.Y + 1}
		}
		return Point{X: p.X, Y: p.Y + 1}
	case NorthEast:
		if p.Y&1 == 0 {
			return Point{X: p.X, Y: p.Y - 1}
		}
		return Point{X: p.X + 1, Y: p.Y - 1}
	case NorthWest:
		if p.Y&1 == 0 {
			return Point{X: p.X - 1, Y: p.Y - 1}
		}
		return Point{X: p.X, Y: p.Y - 1}
	}
	return p
}
