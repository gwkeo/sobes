// 2. Написать генератор случайных чисел

package main

import (
	"fmt"
	"math/rand"
	"time"
)

func randNumsGenerator(n int) <-chan int {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	ch := make(chan int)
	go func() {
		for i := 0; i < n; i++ {
			ch <- r.Intn(n)
		}

		close(ch)
	}()

	return ch
}

func main() {
	for num := range randNumsGenerator(10) {
		fmt.Println(num)
	}
}
