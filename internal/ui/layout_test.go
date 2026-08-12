package ui

import (
	"image"
	"math"
	"testing"

	"gioui.org/f32"
)

func TestOddRowsStayHalfStepOffset(t *testing.T) {
	g := NewBoardGeometry(18, 12, image.Pt(900, 600))
	even, odd := g.DotAt(Point{X: 2, Y: 2}), g.DotAt(Point{X: 2, Y: 3})
	if math.Abs(float64((odd.X-even.X)-g.StepX/2)) > .001 {
		t.Fatalf("odd row offset = %v, want %v", odd.X-even.X, g.StepX/2)
	}
	if math.Abs(float64(odd.Y-even.Y-g.StepY)) > .001 {
		t.Fatalf("row spacing = %v, want %v", odd.Y-even.Y, g.StepY)
	}
}

func TestTrailHalfSegmentsKeepEndpointColors(t *testing.T) {
	g := NewBoardGeometry(4, 4, image.Pt(300, 300))
	got := EndpointHalfSegments(Point{0, 0}, Point{1, 0}, 0xff0000ff, 0x00ff00ff, g)
	if got[0].Color == got[1].Color {
		t.Fatal("endpoint colors were collapsed")
	}
	mid := f32Mid(g.DotAt(Point{0, 0}), g.DotAt(Point{1, 0}))
	if got[0].To != mid || got[1].To != mid {
		t.Fatal("halves do not meet at edge midpoint")
	}
}
func f32Mid(a, b f32.Point) f32.Point { return f32.Pt((a.X+b.X)/2, (a.Y+b.Y)/2) }

func TestBoardGeometryContainsOnlyVisibleBoard(t *testing.T) {
	g := NewBoardGeometry(4, 4, image.Pt(300, 300))
	if !g.ContainsScreen(g.DotAt(Point{X: 2, Y: 2})) {
		t.Fatal("visible board point rejected")
	}
	if g.ContainsScreen(f32.Pt(299, 150)) {
		t.Fatal("navigation-side click accepted as a board action")
	}
}

func TestToroidalWrapTrailStaysAtLocalBoardBoundaries(t *testing.T) {
	g := NewBoardGeometry(18, 18, image.Pt(900, 700))
	got := EndpointHalfSegments(Point{X: 0, Y: 4}, Point{X: 17, Y: 4}, 0xff0000ff, 0x00ff00ff, g)
	for i, segment := range got {
		length := math.Hypot(float64(segment.To.X-segment.From.X), float64(segment.To.Y-segment.From.Y))
		if length > float64(g.StepX) {
			t.Fatalf("wrap half %d crossed board interior: length=%v step=%v segment=%+v", i, length, g.StepX, segment)
		}
	}
}

func TestGeometryFitsMinimumDefaultAndMaximumBoards(t *testing.T) {
	for _, dimensions := range []Point{{X: 4, Y: 4}, {X: 18, Y: 18}, {X: 64, Y: 64}} {
		g := NewBoardGeometry(dimensions.X, dimensions.Y, image.Pt(1280, 720))
		bounds := g.ScreenBounds()
		if bounds.Min.X < 0 || bounds.Min.Y < 0 || bounds.Max.X > 1280 || bounds.Max.Y > 720 {
			t.Fatalf("%dx%d board escaped viewport: %v", dimensions.X, dimensions.Y, bounds)
		}
		if g.DotRadius <= 0 || g.StepX <= 0 || g.StepY <= 0 {
			t.Fatalf("%dx%d board collapsed: %+v", dimensions.X, dimensions.Y, g)
		}
	}
}
