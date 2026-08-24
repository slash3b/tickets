package logger

import (
	"go.uber.org/zap"
)

func MustNew(srvName string, isDebug bool) (*zap.Logger, func() error) {
	l, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}

	if isDebug {
		l, err = zap.NewDevelopment()
		if err != nil {
			panic(err)
		}
	}

	return l.With(zap.String("service_name", srvName)), l.Sync
}
