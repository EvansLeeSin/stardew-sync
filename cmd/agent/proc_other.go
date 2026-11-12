//go:build !unix

package main

func processAlive(pid int) bool { return pid > 0 }
