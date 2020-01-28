package main

import (
	"context"
	"k8s-nlb-registrator-sidecar/constants"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/eapache/go-resiliency/retrier"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/octago/sflags/gen/gflag"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

var (
	app = &App{
		WaitInService:        true,
		WaitInServiceTimeout: 5 * time.Minute,
		PreRegister: &PreRegisterHook{
			Command: "",
			Timeout: 5 * time.Second,
		},
		PostDeregister: &PostDeregisterHook{
			Command: "",
			Timeout: 5 * time.Second,
		},
		PostRegister: &PostRegisterHook{
			Command: "",
			Timeout: 5 * time.Second,
		},
	}
)

type PreRegisterHook struct {
	Command string        `desc:"Command to execute befre target is registered"`
	Timeout time.Duration `desc:"How long to wait for pre-register command to execute"`
}

type PostDeregisterHook struct {
	Command string        `desc:"Command to execute after target is deregistered"`
	Timeout time.Duration `desc:"How long to wait for post-deregister command to execute"`
}

type PostRegisterHook struct {
	Command string        `desc:"Command to execute after target is registered"`
	Timeout time.Duration `desc:"How long to wait for post-register command to execute"`
}

type App struct {
	WaitInService        bool          `desc:"Whether to wait for target group to become healthy"`
	WaitInServiceTimeout time.Duration `desc:"How long to wait for target group to become healthy"`
	TargetID             string        `desc:"Target ID to use"`
	TargetGroupName      string        `desc:"Which target group to use for registering and deregistering targets"`
	TargetGroupArn       string        `flag:"-"`
	PreRegister          *PreRegisterHook
	PostRegister         *PostRegisterHook
	PostDeregister       *PostDeregisterHook
}

func main() {

	logger := setupLogger()

	if err := parseFlags(); err != nil {
		level.Error(logger).Log("error", err)
		os.Exit(1)
	}

	logger = log.With(logger, constants.TargetID, app.TargetID)

	// Graceful shutdown
	stop := signals.SetupSignalHandler()

	// Setup dependencies
	svc := setupELBService(logger)
	registratorService := New(svc, logger)

	var err error
	app.TargetGroupArn, err = discoverTargetGroupArn(app, registratorService)
	logger = log.With(logger, constants.TargetGroupArn, app.TargetGroupArn)

	// TODO: Fix log context propagation
	registratorService.Logger = logger

	if err != nil {
		logger.Log("error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	regCancelCtx, regCancelFunc := context.WithCancel(ctx)
	// Passing cancellable context in case app.WaitInService is true and we
	// intentionally sent SIGINT/SIGTERM to the program.
	// It doesn't make sense to wait for target to be in service when we actually
	// want to deregister it from target group
	go registerTarget(regCancelCtx, app, registratorService, logger)

	// Block and wait for signal
	logger.Log("msg", "Awaiting signal for deregistration")
	<-stop
	// Cancel Register Target operation if case it's running
	regCancelFunc()

	// Deregister Target in Target Group
	deregisterTarget(ctx, app, registratorService, logger)
}

func setupLogger() log.Logger {
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "time", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	return logger
}

func parseFlags() error {
	fs, err := gflag.Parse(app)
	if err != nil {
		return err
	}

	err = fs.Parse(os.Args[1:])
	if err != nil {
		return err
	}
	return nil
}

func setupELBService(logger log.Logger) *elbv2.ELBV2 {
	var sess *session.Session
	sessionRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := sessionRetrier.Run(func() error {
		var err error
		sess, err = session.NewSession()
		return err
	})

	if err != nil {
		level.Error(logger).Log("error", err)
		os.Exit(1)
	}
	return elbv2.New(sess)
}

func discoverTargetGroupArn(app *App, registratorService *RegistratorService) (string, error) {
	var targetGroupArn string
	discoverTargetGroupArnRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := discoverTargetGroupArnRetrier.Run(func() error {
		var err error
		targetGroupArn, err = registratorService.DiscoverTargetGroupArn(app.TargetGroupName)
		return err
	})
	if err != nil {
		return "", err
	}

	return targetGroupArn, nil
}

func registerTarget(ctx context.Context, app *App, registratorService *RegistratorService, logger log.Logger) {
	if app.PreRegister.Command != "" {
		preRegCtx, preRegCancelFn := context.WithTimeout(ctx, app.PreRegister.Timeout)
		defer preRegCancelFn()
		logger.Log("msg", "Executing pre-register command", "command", app.PreRegister.Command)
		ExecCommand(preRegCtx, logger, app.PreRegister.Command)
	}

	registerRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := registerRetrier.RunCtx(ctx, func(ctx context.Context) error {
		return registratorService.RegisterTarget(ctx, &RegisterTargetInput{
			ID:                        aws.String(app.TargetID),
			TargetGroupArn:            aws.String(app.TargetGroupArn),
			WaitUntilInService:        aws.Bool(app.WaitInService),
			WaitUntilInServiceTimeout: app.WaitInServiceTimeout,
		})
	})

	if err != nil {
		logger.Log("error", err)
	}

	if app.PostRegister.Command != "" {
		postRegCtx, postRegCancelFn := context.WithTimeout(ctx, app.PostRegister.Timeout)
		defer postRegCancelFn()
		logger.Log("msg", "Executing post-register command", "command", app.PostRegister.Command)
		ExecCommand(postRegCtx, logger, app.PostRegister.Command)
	}

}

func deregisterTarget(ctx context.Context, app *App, registratorService *RegistratorService, logger log.Logger) {
	deregisterRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := deregisterRetrier.RunCtx(ctx, func(context.Context) error {
		err := registratorService.DeregisterTarget(ctx, &DeregisterTargetInput{
			ID:             aws.String(app.TargetID),
			TargetGroupArn: aws.String(app.TargetGroupArn),
		})
		if err != nil {
			level.Error(logger).Log("error", err)
		}
		return err
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
