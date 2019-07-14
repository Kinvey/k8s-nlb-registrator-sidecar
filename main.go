package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/eapache/go-resiliency/retrier"
	"github.com/go-kit/kit/log"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

var (
	app *App = &App{
		PreRegister:    &Hook{},
		PostDeregister: &Hook{},
	}
)

type Hook struct {
	Command string
	Timeout time.Duration
}

type App struct {
	WaitInService        bool
	WaitInServiceTimeout time.Duration
	TargetID             string
	TargetGroupName      string
	PreRegister          *Hook
	PostDeregister       *Hook
}

func init() {
	flag.BoolVar(&app.WaitInService, "wait-in-service", true, "Whether to wait for NLB target to become healthy")
	flag.DurationVar(&app.WaitInServiceTimeout, "wait-in-service-timeout", 5*time.Minute, "How long to wait for NLB target to become healthy")
	flag.StringVar(&app.TargetID, "target-id", "", "TargetID to register in NLB")
	flag.StringVar(&app.TargetGroupName, "target-group-name", "", "Target group name to look for")
	flag.StringVar(&app.PostDeregister.Command, "post-deregister-command", "", "Command to execute after target is deregistered from target group")
	flag.DurationVar(&app.PostDeregister.Timeout, "post-deregister-command-timeout", 5*time.Second, "How long to wait for pre-deregister-command to finish")
	flag.StringVar(&app.PreRegister.Command, "pre-register-command", "", "Command to execute befre target is registered with target group")
	flag.DurationVar(&app.PreRegister.Timeout, "pre-register-command-timeout", 5*time.Second, "How long to wait for pre-register-command to finish")
}

func main() {
	// Setup flags
	flag.Parse()

	var logger log.Logger
	logger = log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "time", log.DefaultTimestampUTC, "caller", log.DefaultCaller)

	ctx := context.Background()
	// Setup graceful shutdown
	done := signals.SetupSignalHandler()

	var sess *session.Session

	// Setup dependencies
	sessionRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := sessionRetrier.Run(func() error {
		var err error
		sess, err = session.NewSession()
		return err
	})

	if err != nil {
		logger.Log("error", err)
	}

	svc := elbv2.New(sess)
	registratorService := New(svc, logger)

	var targetGroupArn string
	discoverTargetGroupArnRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err = discoverTargetGroupArnRetrier.Run(func() error {
		var err error
		targetGroupArn, err = registratorService.DiscoverTargetGroupArn(app.TargetGroupName)
		return err
	})

	if err != nil {
		logger.Log("error", err)
		os.Exit(1)
	}

	regCancelCtx, regCancelFunc := context.WithCancel(ctx)

	go func() {
		if app.PreRegister.Command != "" {
			preRegCtx, preRegCancelFn := context.WithTimeout(regCancelCtx, app.PreRegister.Timeout)
			defer preRegCancelFn()
			logger.Log("msg", "Executing pre-register command", "command", app.PreRegister.Command)
			ExecCommand(preRegCtx, logger, app.PreRegister.Command)
		}

		registerRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
		err = registerRetrier.RunCtx(regCancelCtx, func(ctx context.Context) error {
			return registratorService.RegisterTarget(ctx, &RegisterTargetInput{
				ID:                        aws.String(app.TargetID),
				TargetGroupArn:            aws.String(targetGroupArn),
				WaitUntilInService:        aws.Bool(app.WaitInService),
				WaitUntilInServiceTimeout: app.WaitInServiceTimeout,
			})
		})

		if err != nil {
			logger.Log("error", err)
		}
	}()

	logger.Log("msg", "Awaiting signal for deregistration")

	// Block and wait for signal
	<-done
	regCancelFunc()

	deregisterRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err = deregisterRetrier.RunCtx(ctx, func(context.Context) error {
		return registratorService.DeregisterTarget(ctx, &DeregisterTargetInput{
			ID:             aws.String(app.TargetID),
			TargetGroupArn: aws.String(targetGroupArn),
		})
	})

	if err != nil {
		logger.Log("error", err)
	}

	ctx, cancel := context.WithTimeout(ctx, app.PostDeregister.Timeout)
	defer cancel()

	if app.PostDeregister.Command != "" {
		logger.Log("msg", "Executing post-deregister command", "command", app.PostDeregister.Command)
		ExecCommand(ctx, logger, app.PostDeregister.Command)
	}
}
