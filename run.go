package ecspresso

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/pkg/errors"
)

func (d *App) Run(opt RunOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	d.Log("Running task", opt.DryRunString())
	ov := ecs.TaskOverride{}
	if ovStr := aws.StringValue(opt.TaskOverrideStr); ovStr != "" {
		if err := json.Unmarshal([]byte(ovStr), &ov); err != nil {
			return errors.Wrap(err, "invalid overrides")
		}
	} else if ovFile := aws.StringValue(opt.TaskOverrideFile); ovFile != "" {
		src, err := d.readDefinitionFile(ovFile)
		if err != nil {
			return errors.Wrapf(err, "failed to read overrides-file %s", ovFile)
		}
		if err := d.unmarshalJSON(src, &ov, ovFile); err != nil {
			return errors.Wrapf(err, "failed to read overrides-file %s", ovFile)
		}
	}
	d.DebugLog("Overrides:", ov.String())

	tdArn, err := d.taskDefinitionArnForRun(ctx, opt)
	if err != nil {
		return err
	}
	d.Log("Task definition ARN:", tdArn)
	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}
	td, err := d.DescribeTaskDefinition(ctx, tdArn)
	if err != nil {
		return err
	}
	watchContainer := containerOf(td, opt.WatchContainer)
	d.Log("Watch container:", *watchContainer.Name)

	task, err := d.RunTask(ctx, tdArn, &ov, &opt)
	if err != nil {
		return errors.Wrap(err, "failed to run task")
	}
	if *opt.NoWait {
		d.Log("Run task invoked")
		return nil
	}
	if err := d.WaitRunTask(ctx, task, watchContainer, time.Now(), opt.waitUntilRunning()); err != nil {
		return errors.Wrap(err, "failed to run task")
	}
	if err := d.DescribeTaskStatus(ctx, task, watchContainer); err != nil {
		return err
	}
	d.Log("Run task completed!")

	return nil
}

func (d *App) RunTask(ctx context.Context, tdArn string, ov *ecs.TaskOverride, opt *RunOption) (*ecs.Task, error) {
	d.Log("Running task with", tdArn)

	sv, err := d.LoadServiceDefinition(d.config.ServiceDefinitionPath)
	if err != nil {
		return nil, err
	}

	tags, err := parseTags(*opt.Tags)
	if err != nil {
		return nil, err
	}

	in := &ecs.RunTaskInput{
		Cluster:                  aws.String(d.Cluster),
		TaskDefinition:           aws.String(tdArn),
		NetworkConfiguration:     sv.NetworkConfiguration,
		LaunchType:               sv.LaunchType,
		Overrides:                ov,
		Count:                    opt.Count,
		CapacityProviderStrategy: sv.CapacityProviderStrategy,
		PlacementConstraints:     sv.PlacementConstraints,
		PlacementStrategy:        sv.PlacementStrategy,
		PlatformVersion:          sv.PlatformVersion,
		Tags:                     tags,
		EnableECSManagedTags:     sv.EnableECSManagedTags,
		EnableExecuteCommand:     sv.EnableExecuteCommand,
	}

	switch aws.StringValue(opt.PropagateTags) {
	case "SERVICE":
		out, err := d.ecs.ListTagsForResourceWithContext(ctx, &ecs.ListTagsForResourceInput{
			ResourceArn: sv.ServiceArn,
		})
		if err != nil {
			return nil, err
		}
		d.DebugLog("propagate tags from service", *sv.ServiceArn, out.String())
		for _, tag := range out.Tags {
			in.Tags = append(in.Tags, tag)
		}
	case "":
		in.PropagateTags = nil
	default:
		in.PropagateTags = opt.PropagateTags
	}
	d.DebugLog("run task input", in.String())

	out, err := d.ecs.RunTaskWithContext(ctx, in)
	if err != nil {
		return nil, err
	}
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		if f.Arn != nil {
			d.Log("Task ARN: " + *f.Arn)
		}
		return nil, errors.New(*f.Reason)
	}

	task := out.Tasks[0]
	d.Log("Task ARN:", *task.TaskArn)
	return task, nil
}

func (d *App) WaitRunTask(ctx context.Context, task *ecs.Task, watchContainer *ecs.ContainerDefinition, startedAt time.Time, untilRunning bool) error {
	d.Log("Waiting for run task...(it may take a while)")
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	lc := watchContainer.LogConfiguration
	if lc == nil || *lc.LogDriver != "awslogs" || lc.Options["awslogs-stream-prefix"] == nil {
		d.Log("awslogs not configured")
		if err := d.waitTask(ctx, task, untilRunning); err != nil {
			return errors.Wrap(err, "failed to run task")
		}
		return nil
	}

	d.Log(fmt.Sprintf("Watching container: %s", *watchContainer.Name))
	logGroup, logStream := d.GetLogInfo(task, watchContainer)
	time.Sleep(3 * time.Second) // wait for log stream

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		var nextToken *string
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-ticker.C:
				nextToken, _ = d.GetLogEvents(waitCtx, logGroup, logStream, startedAt, nextToken)
			}
		}
	}()

	if err := d.waitTask(ctx, task, untilRunning); err != nil {
		return errors.Wrap(err, "failed to run task")
	}
	return nil
}

func (d *App) waitTask(ctx context.Context, task *ecs.Task, untilRunning bool) error {
	// Add an option WithWaiterDelay and request.WithWaiterMaxAttempts for a long timeout.
	// SDK Default is 10 min (MaxAttempts=100 * Delay=6sec) at now.
	const delay = 6 * time.Second
	attempts := int((d.config.Timeout / delay)) + 1
	if (d.config.Timeout % delay) > 0 {
		attempts++
	}

	id := arnToName(*task.TaskArn)
	if untilRunning {
		d.Log(fmt.Sprintf("Waiting for task ID %s until running", id))
		if err := d.ecs.WaitUntilTasksRunningWithContext(
			ctx,
			d.DescribeTasksInput(task),
			request.WithWaiterDelay(request.ConstantWaiterDelay(delay)),
			request.WithWaiterMaxAttempts(attempts),
		); err != nil {
			return err
		}
		d.Log(fmt.Sprintf("Task ID %s is running", id))
		return nil
	}

	d.Log(fmt.Sprintf("Waiting for task ID %s until stopped", id))
	return d.ecs.WaitUntilTasksStoppedWithContext(
		ctx, d.DescribeTasksInput(task),
		request.WithWaiterDelay(request.ConstantWaiterDelay(delay)),
		request.WithWaiterMaxAttempts(attempts),
	)
}

func (d *App) taskDefinitionArnForRun(ctx context.Context, opt RunOption) (string, error) {
	switch {
	case *opt.SkipTaskDefinition, *opt.LatestTaskDefinition:
		var family string
		if d.config.Service != "" {
			sv, err := d.DescribeService(ctx)
			if err != nil {
				return "", err
			}
			tdArn := *sv.TaskDefinition
			p := strings.SplitN(arnToName(tdArn), ":", 2)
			family = p[0]
		} else {
			in, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
			if err != nil {
				return "", err
			}
			family = *in.Family
		}
		if rev := aws.Int64Value(opt.Revision); rev > 0 {
			return fmt.Sprintf("%s:%d", family, rev), nil
		}

		d.Log("Revision is not specified. Use latest task definition family" + family)
		latestTdArn, err := d.findLatestTaskDefinitionArn(ctx, family)
		if err != nil {
			return "", err
		}
		return latestTdArn, nil
	default:
		tdPath := aws.StringValue(opt.TaskDefinition)
		if tdPath == "" {
			tdPath = d.config.TaskDefinitionPath
		}
		in, err := d.LoadTaskDefinition(tdPath)
		if err != nil {
			return "", err
		}
		if *opt.DryRun {
			return fmt.Sprintf("family %s will be registered", *in.Family), nil
		}
		newTd, err := d.RegisterTaskDefinition(ctx, in)
		if err != nil {
			return "", err
		}
		return *newTd.TaskDefinitionArn, nil
	}
}
