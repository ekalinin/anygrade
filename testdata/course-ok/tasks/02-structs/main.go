package main

// Point is a simple 2D point.
type Point struct {
	X, Y int
}

// Add returns the sum of two points.
func Add(a, b Point) Point {
	return Point{X: a.X + b.X, Y: a.Y + b.Y}
}

func main() {}
