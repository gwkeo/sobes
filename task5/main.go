// 5.Написать WorkerPool с заданной функцией
// Довольно распространенная задача, плюс подобные задачи встречаются на практике.
// Нам нужно разбить процессы на несколько горутин — при этом не создавать новую горутину каждый раз, а просто переиспользовать уже имеющиеся.
// Для этого создадим канал с джобами и результирующий канал.
// Для каждого воркера создадим горутину, который будет ждать новую джобу, применять к ней заданную функцию и пулять ответ в результирующий канал.

package main

import "fmt"

func worker(fn func(int) int, jobs <-chan int, results chan<- int) {
	for j := range jobs {
		results <- fn(j)
	}
}

func main() {
	numJobs := 100
	jobs := make(chan int, numJobs)
	results := make(chan int, numJobs)

	for i := 0; i < 3; i++ {
		go worker(func(a int) int {
			return a * 10
		}, jobs, results)
	}

	for i := 1; i <= numJobs; i++ {
		jobs <- i
	}
	close(jobs)

	for range numJobs {
		fmt.Println(<-results)
	}
}
