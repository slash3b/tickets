package fetcher

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestFoo(t *testing.T) {
	fmt.Println(gen(9))
}

func gen(i int) []time.Duration {
	res := make([]time.Duration, i)

	for r := range i {
		res[r] = time.Duration(100<<r+rand.Intn(200)) * time.Millisecond
	}

	return res
}
