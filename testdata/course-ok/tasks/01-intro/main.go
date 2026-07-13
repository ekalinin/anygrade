package main

import "fmt"

// Greet returns a greeting for the given name.
func Greet(name string) string {
	return "Hello, " + name + "!"
}

func main() {
	fmt.Println(Greet("world"))
}
