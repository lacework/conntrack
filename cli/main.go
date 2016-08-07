package main

// Example usage

import (
	"fmt"
	"time"

	"github.com/lacework/conntrack"
)

func main() {
	cs, _, err := conntrack.Established()
	if err != nil {
		panic(err)
	}
	fmt.Printf("Established on start:\n")
	for _, cn := range cs {
		fmt.Printf(" - %s\n", cn)
	}
	fmt.Println("")

	c, err := conntrack.New()
	if err != nil {
		panic(err)
	}
	for range time.Tick(1 * time.Second) {
		fmt.Printf("Connections:\n")
		for _, cn := range c.Connections() {
			fmt.Printf(" - %s\n", cn)
		}
	}
}
