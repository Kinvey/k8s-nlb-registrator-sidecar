package main

type Logger interface {
	Fatalln(v ...interface{})
	Infof(string, ...interface{})
	Infoln(v ...interface{})
}
