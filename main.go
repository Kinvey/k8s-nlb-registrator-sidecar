package main

import (
	"context"
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

type App struct {
	WaitInService        bool          `desc:"Whether to wait for target group to become healthy"`
	WaitInServiceTimeout time.Duration `desc:"How long to wait for target group to become healthy"`
	TargetID             string        `desc:"Target ID to use"`
	TargetGroupName      string        `desc:"Which target group to use for registering and deregistering targets"`
	TargetGroupArn       string        `flag:"-"`
	PreRegister          *PreRegisterHook
	PostDeregister       *PostDeregisterHook
}

// func init() {
// 	flag.BoolVar(&app.WaitInService, "wait-in-service", true, "Whether to wait for NLB target to become healthy")
// 	flag.DurationVar(&app.WaitInServiceTimeout, "wait-in-service-timeout", 5*time.Minute, "How long to wait for NLB target to become healthy")
// 	flag.StringVar(&app.TargetID, "target-id", "", "TargetID to register in NLB")
// 	flag.StringVar(&app.TargetGroupName, "target-group-name", "", "Target group name to look for")
// 	flag.StringVar(&app.PostDeregister.Command, "post-deregister-command", "", "Command to execute after target is deregistered from target group")
// 	flag.DurationVar(&app.PostDeregister.Timeout, "post-deregister-command-timeout", 5*time.Second, "How long to wait for pre-deregister-command to finish")
// 	flag.StringVar(&app.PreRegister.Command, "pre-register-command", "", "Command to execute befre target is registered with target group")
// 	flag.DurationVar(&app.PreRegister.Timeout, "pre-register-command-timeout", 5*time.Second, "How long to wait for pre-register-command to finish")
// }

func RegisterTarget(ctx context.Context, app *App, registratorService *RegistratorService, logger log.Logger) {
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
}

func DeregisterTarget(ctx context.Context, app *App, registratorService *RegistratorService, logger log.Logger) {
	deregisterRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := deregisterRetrier.RunCtx(ctx, func(context.Context) error {
		return registratorService.DeregisterTarget(ctx, &DeregisterTargetInput{
			ID:             aws.String(app.TargetID),
			TargetGroupArn: aws.String(app.TargetGroupArn),
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

func DiscoverTargetGroupArn(app *App, registratorService *RegistratorService, logger log.Logger) string {
	var targetGroupArn string
	discoverTargetGroupArnRetrier := retrier.New(retrier.ExponentialBackoff(3, 1*time.Second), nil)
	err := discoverTargetGroupArnRetrier.Run(func() error {
		var err error
		targetGroupArn, err = registratorService.DiscoverTargetGroupArn(app.TargetGroupName)
		return err
	})
	if err != nil {
		logger.Log("error", err)
		os.Exit(1)
	}

	return targetGroupArn
}

func SetupLogger() log.Logger {
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "time", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	return logger
}

func ParseFlags() error {
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

func SetupELBService(logger log.Logger) *elbv2.ELBV2 {
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

func main() {

	logger := SetupLogger()

	if err := ParseFlags(); err != nil {
		level.Error(logger).Log("error", err)
	}

	ctx := context.Background()
	// Setup graceful shutdown
	done := signals.SetupSignalHandler()

	// Setup dependencies
	svc := SetupELBService(logger)
	registratorService := New(svc, logger)
	// Discover TargetGroupArn
	app.TargetGroupArn = DiscoverTargetGroupArn(app, registratorService, logger)

	// Register Target in Target Group
	regCancelCtx, regCancelFunc := context.WithCancel(ctx)
	go RegisterTarget(regCancelCtx, app, registratorService, logger)

	logger.Log("msg", "Awaiting signal for deregistration")

	// Block and wait for signal
	<-done
	regCancelFunc()

	DeregisterTarget(ctx, app, registratorService, logger)
}
