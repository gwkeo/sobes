// 6. Сделать кастомную waitGroup на семафоре
// Семафор можно легко получить из канала. Чтоб не аллоцировать лишние данные, будем складывать туда пустые структуры.
// В нашем случае мы хотим сделать семафор, который будет ждать выполнения пяти горутин.
// Для этого просто добавим вместо обычного канала буфферизированный.
// И внутри каждой горутины положим в него значение.
// А в конце будем дожидаться, что все ок — мы вычитаем все значения из канала.

package main

import "fmt"

type wg struct {
	ch      chan struct{}
	counter int
}

func New() wg {
	return wg{ch: make(chan struct{}, 1)}
}

func (w *wg) Add(a int) {
	w.counter += a
}

func (w *wg) Done() {
	w.counter -= 1
}

func (w *wg) Wait() {
	go func() {
		for w.counter > 0 {
		}
		w.ch <- struct{}{}
	}()

	<-w.ch
}

func main() {
	wg := New()

	wg.Add(2)
	go func() {
		defer wg.Done()

		for i := range 100 {
			fmt.Println("1: ", i)
		}
	}()

	go func() {
		defer wg.Done()

		for i := range 100 {
			fmt.Println("2: ", i*2)
		}
	}()

	wg.Wait()
}
