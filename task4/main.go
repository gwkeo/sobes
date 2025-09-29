// 4. Сделать конвейер чисел
// Даны два канала. В первый пишутся числа. Нужно, чтобы числа
// читались из первого по мере поступления, что-то с ними
// происходило (допустим, возводились в квадрат) и результат
// записывался во второй канал.

package main

import (
	"fmt"
)

func reader(ch <-chan int) {
	for v := range ch {
		fmt.Println(v)
	}
}

func writer() <-chan int {
	ch := make(chan int)

	go func() {
		for i := range 10 {
			ch <- i
		}
		close(ch)
	}()

	return ch
}

func powerTwo(ch <-chan int) <-chan int {
	resCh := make(chan int)
	go func() {
		for i := range ch {
			resCh <- i * i
		}
		close(resCh)
	}()

	return resCh
}

func main() {
	reader(powerTwo(writer()))
}
