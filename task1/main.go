// 1. На вход подаются два неупорядоченных слайса любой длины.
// Надо написать функцию, которая возвращает их пересечение

package main

import "fmt"

func main() {
	a, b := []int{1, 2, 5, 8, 20}, []int{2, 3, 7, 10, 20, 22}
	fmt.Println(a, b)

	m := make(map[int]struct{})

	for i := 0; i < len(a); i++ {
		m[a[i]] = struct{}{}
	}

	res := []int{}

	for i := 0; i < len(b); i++ {
		if _, ok := m[b[i]]; ok {
			res = append(res, b[i])
		}
	}

	fmt.Println(res)
}
