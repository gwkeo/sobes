// 3. Слить N каналов в один
// Даны n каналов типа chan int. Надо написать функцию, которая
// смерджит все данные из этих каналов в один и вернет его.
// Мы хотим, чтобы результат работы функции выглядел примерно так:
// for num := range joinChannels(a, b, c) {
//       fmt.Println(num)
// }

package main

import (
	"fmt"
	"sync"
)

func main() {
	a := make(chan int)
	b := make(chan int)
	c := make(chan int)

	go func() {
		for _, v := range []int{100, 700, 199} {
			a <- v
		}
		close(a)
	}()

	go func() {
		for _, v := range []int{1, 2, 7} {
			b <- v
		}
		close(b)
	}()

	go func() {
		for _, v := range []int{30, 40, 60} {
			c <- v
		}
		close(c)
	}()

	for v := range joinChans(a, b, c) {
		fmt.Println(v)
	}
}

func joinChans(channels ...<-chan int) <-chan int {
	res := make(chan int)
	go func() {
		wg := &sync.WaitGroup{}
		wg.Add(len(channels))

		for _, channel := range channels {
			go func(ch <-chan int, wg *sync.WaitGroup) {
				defer wg.Done()

				for v := range channel {
					res <- v
				}
			}(channel, wg)
		}

		wg.Wait()
		close(res)
	}()

	return res
}
