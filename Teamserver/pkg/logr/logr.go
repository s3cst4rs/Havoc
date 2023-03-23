package logr

import (
	"os"

	"Havoc/pkg/logger"
)

type Logr struct {
	// Path to directory where everything is going to be logged (user chat, input/output from agent)
	Path         string // 日志路径
	ListenerPath string // Listener日志路径
	AgentPath    string // Agent日志路径
	ServerPath   string // 当前路径

	LogrSendText func(text string)
}

var LogrInstance *Logr

// 创建所有日志相关的路径
// Server 当前路径 Path 日志路径
func NewLogr(Server, Path string) *Logr {
	var (
		logr = new(Logr)
		err  error
	)

	logr.ServerPath = Server
	logr.Path = Server + "/" + Path
	logr.ListenerPath = Path + "/listener"
	logr.AgentPath = Path + "/agents"

	// 新建日志路径
	if _, err = os.Stat(Path); os.IsNotExist(err) {
		if err = os.MkdirAll(Path, os.ModePerm); err != nil {
			logger.Error("Failed to create Logr folder: " + err.Error())
			return nil
		}
	} else {
		err = os.RemoveAll(Path)
		if err == nil {
			if err = os.MkdirAll(Path, os.ModePerm); err != nil {
				logger.Error("Failed to create Logr folder: " + err.Error())
				return nil
			}
		} else {
			logger.Error(err.Error())
		}
	}

	if _, err = os.Stat(logr.AgentPath); os.IsNotExist(err) {
		if err = os.MkdirAll(logr.AgentPath, os.ModePerm); err != nil {
			logger.Error("Failed to create Logr agent folder: " + err.Error())
			return nil
		}
	}

	if _, err = os.Stat(logr.ListenerPath); os.IsNotExist(err) {
		if err = os.MkdirAll(logr.ListenerPath, os.ModePerm); err != nil {
			logger.Error("Failed to create Logr listener folder: " + err.Error())
			return nil
		}
	}

	return logr
}
