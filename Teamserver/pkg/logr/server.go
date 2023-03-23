package logr

import (
	"bufio"
	"log"
	"os"
	"regexp"

	"Havoc/pkg/logger"
)

func strip(str []byte) []byte {
	var (
		ansi = "[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))"
		re   = regexp.MustCompile(ansi)
	)

	return []byte(re.ReplaceAllString(string(str), ""))
}

// 将日志写入文件并打印，同时会广播到所有的Client
func (l Logr) ServerStdOutInit() {
	var (
		PathStdOut = l.Path + "/teamserver.log"
		OldStdout  = os.Stdout

		StdRead, StdWrite, _ = os.Pipe()
	)

	File, err := os.OpenFile(PathStdOut, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}

	os.Stdout = StdWrite

	logger.LoggerInstance = logger.NewLogger(StdWrite)

	go func() {
		var Reader = bufio.NewReader(StdRead)

		for {
			if Reader.Size() > 0 {
				var (
					rawLine, _, _ = Reader.ReadLine()
					line          = []byte(string(rawLine) + "\n")
				)

				// LogrSendText是日志发送函数
				if l.LogrSendText != nil {
					// 日志广播
					l.LogrSendText(string(strip(rawLine)))
				}

				// 写入日志文件
				_, err := File.Write(strip(line))
				if err != nil {
					return
				}

				// 打印到标准输出
				_, err = OldStdout.Write(line)
				if err != nil {
					return
				}
			}
		}
	}()
}
