package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bndr/gojenkins"
	"github.com/pkg/errors"
)

func getInt64FromEnv(varName string, def int64) int64 {
	envVal := os.Getenv(varName)
	if envVal == "" {
		return def
	}
	parsed, err := strconv.ParseInt(envVal, 10, 64)
	if err != nil {
		fmt.Println(errors.Wrapf(err, "failed to convert %s(%s) to int", varName, envVal))
		os.Exit(1)
	}
	return parsed
}

func getDurationFromEnv(varName string, def time.Duration) time.Duration {
	envValue := os.Getenv(varName)
	if envValue == "" {
		return def
	}
	parsed, err := strconv.Atoi(envValue)
	if err != nil {
		fmt.Println(errors.Wrapf(err, "failed to convert %s(%s) to int", varName, envValue))
		os.Exit(1)
	}
	return time.Duration(parsed)
}

func repeatNTimes(limit int, fn func() error) error {
	for errCnt := 0; ; errCnt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if errCnt == limit {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readBuildOutput(
	ctx context.Context,
	output, errOutput io.Writer,
	currentOutputID int64,
	build *gojenkins.Build,
) (int64, error) {
	hasMoreText := true
	for hasMoreText {
		err := repeatNTimes(5, func() error {
			resp, err := build.GetConsoleOutputFromIndex(ctx, currentOutputID)
			if err != nil {
				err = errors.Wrapf(err, "failed to get console output")
				fmt.Fprintln(errOutput, err)
				return err
			}
			currentOutputID = resp.Offset
			hasMoreText = resp.HasMoreText
			if len(resp.Content) != 0 {
				fmt.Fprint(output, resp.Content)
			}
			return nil
		})
		if err != nil {
			return currentOutputID, err
		}
	}
	return currentOutputID, nil
}

func readBuildState(ctx context.Context, errOutput io.Writer, build *gojenkins.Build) error {
	return repeatNTimes(5, func() error {
		_, err := build.Poll(ctx)
		if err == nil {
			return nil
		}
		err = errors.Wrapf(err, "failed to read build state")
		fmt.Fprintln(errOutput, err)
		return err
	})
}

func waitBuildToComplete(
	ctx context.Context,
	output, errOutput io.Writer,
	poolingInterval time.Duration,
	build *gojenkins.Build,
) error {
	var err error
	currentOutputID := int64(0)
	for range time.NewTicker(poolingInterval).C {
		select {
		case <-ctx.Done():
			return errors.Errorf("reached timeout on waiting for build results")
		default:
		}
		if output != nil {
			currentOutputID, err = readBuildOutput(ctx, output, errOutput, currentOutputID, build)
			if err != nil {
				return err
			}
		}
		if err = readBuildState(ctx, errOutput, build); err != nil {
			return err
		}
		if !build.IsRunning(ctx) {
			break
		}
	}
	return nil
}

func waitForJobToBePickedUp(
	ctx context.Context,
	jenkins *gojenkins.Jenkins,
	poolingInterval time.Duration,
	queueID int64,
) (build *gojenkins.Build, err error) {
	var task *gojenkins.Task
	var job *gojenkins.Job
	// ctx := context.WithValue(ctx, "debug", true)

	for range time.NewTicker(poolingInterval).C {
		select {
		case <-ctx.Done():
			return nil, errors.Errorf("reached timeout on waiting for build results")
		default:
		}

		task, err = jenkins.GetQueueItem(ctx, queueID)
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				continue
			}
			return nil, err
		}

		_, err = task.Poll(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				continue
			}
			return nil, err
		}
		if task.Raw.Executable.Number == 0 {
			continue
		}

		first := strings.Index(task.Raw.Task.URL, "/job/") + len("/job/")
		parents := strings.Split(task.Raw.Task.URL[first:], "/job/")
		job, err = jenkins.GetJob(ctx, parents[len(parents)-1], parents[:len(parents)-1]...)
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				continue
			}
			return nil, err
		}
		build, err = job.GetBuild(ctx, task.Raw.Executable.Number)
		if err == nil {
			return build, nil
		}
		if strings.Contains(err.Error(), "404") {
			continue
		}
		return nil, err
	}
	return nil, errors.Wrap(err, "timeout reached on waiting for build to be picked up, last error")
}

var waitIndefinitely = time.Duration(1<<63 - 1)

func execute(
	ctx context.Context,
	baseURL, user, password, jobName, jobParameters string,
	buildID int64,
	waitTimeout, poolingInterval time.Duration,
) error {
	// Disable SSL verification
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	jenkins := gojenkins.CreateJenkins(nil, baseURL, user, password)
	_, err := jenkins.Init(ctx)
	if err != nil {
		return errors.Wrapf(err, "Failed to initialize jenkins client")
	}

	var build *gojenkins.Build
	if buildID != 0 {
		build, err = getCurrentBuild(ctx, jenkins, jobName, buildID)
		if err != nil {
			return err
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		build, err = startNewBuild(ctx, jenkins, jobName, jobParameters, poolingInterval)
		if err != nil {
			return err
		}
	}

	err = waitBuildToComplete(ctx, os.Stdout, os.Stderr, poolingInterval, build)
	if err != nil {
		return errors.Wrapf(err, "Failed to wait for build to complete")
	}

	if build.IsGood(ctx) {
		fmt.Fprintf(os.Stdout, "Job %s successfully completed, URL: %s\n", jobName, build.GetUrl())
		return nil
	}
	return errors.Errorf("Job %s failed, URL: %s", jobName, build.GetUrl())
}

func getCurrentBuild(
	ctx context.Context,
	jenkins *gojenkins.Jenkins,
	jobName string,
	buildID int64,
) (*gojenkins.Build, error) {
	job, err := jenkins.GetJob(ctx, jobName)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get job %s", jobName)
	}

	build, err := job.GetBuild(ctx, buildID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get build %d", buildID)
	}

	fmt.Fprintf(os.Stdout, "Job %s with build id %d is found\n", jobName, build.Raw.Number)
	return build, nil
}

func startNewBuild(
	ctx context.Context,
	jenkins *gojenkins.Jenkins,
	jobName string,
	jobParameters string,
	poolingInterval time.Duration,
) (*gojenkins.Build, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Wrapf(ctx.Err(), "Timeout reached on waiting for job to be picked up")
		default:
		}

		job, err := jenkins.GetJob(ctx, jobName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to get job %s", jobName)
		}

		parsedJobParameters := map[string]string{}
		if jobParameters != "" {
			err = json.Unmarshal([]byte(jobParameters), &parsedJobParameters)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed to parse job parameters")
			}
		}

		queueID, err := job.InvokeSimple(ctx, parsedJobParameters) // or  jenkins.BuildJob(ctx, "#jobname", params)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to invoke job %s", jobName)
		}

		if queueID == 0 {
			// Job runner is already reused, need to acquire a new one
			time.Sleep(1 * time.Second)
			continue
		}

		fmt.Fprintf(os.Stdout, "Job %s is queued with id %d, waiting for working to pick it up\n", jobName, queueID)
		build, err := waitForJobToBePickedUp(ctx, jenkins, poolingInterval, queueID)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to wait for job to be picked up")
		}

		fmt.Fprintf(os.Stdout, "Job %s is running with build id %d\n", jobName, build.Raw.Number)
		return build, nil
	}
}

func main() {
	baseURL := flag.String(
		"base-url",
		os.Getenv("JENKINS_BASE_URL"),
		"A jenkins http base url. Example: https://jenkins.my-org.com")
	user := flag.String(
		"user",
		os.Getenv("JENKINS_USER"),
		"A jenkins user to authenticate: Example: my-email@my-org.com")
	password := flag.String(
		"password",
		os.Getenv("JENKINS_PASSWORD"),
		"A password.")
	jobName := flag.String(
		"job-name",
		os.Getenv("JENKINS_JOB_NAME"),
		"A jenkins name you want to run. Example: my_folder/my_job")
	buildID := flag.Int64(
		"build-id",
		getInt64FromEnv("JENKINS_JOB_BUILD_ID", 0), "An build ID of a run to watch. Example: 25")
	jobParameters := flag.String(
		"job-parameters",
		os.Getenv("JENKINS_JOB_PARAMETERS"),
		"A jenkins job parameters. Example: my_folder/my_job")
	waitTimeout := flag.Duration(
		"wait-timeout",
		getDurationFromEnv("JENKINS_WAIT_TIMEOUT", -1),
		"A waiting timeout. Default - Wait indefinitely, 0 - do not wait. Default is 0. Example: my_folder/my_job")
	waitPoolingInterval := flag.Duration(
		"wait-pooling-interval",
		getDurationFromEnv("JENKINS_WAIT_POOLING_INTERVAL", 500*time.Millisecond),
		"A pooling interval when wait")

	flag.Parse()

	var errs []string

	if *baseURL == "" {
		errs = append(errs, "base-url is empty")
	}
	if *user == "" {
		errs = append(errs, "user is empty")
	}
	if *password == "" {
		errs = append(errs, "password is empty")
	}
	if *jobName == "" {
		errs = append(errs, "job-name is empty")
	}

	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "Errors:")
		for _, err := range errs {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}

	if *waitTimeout == -1 {
		waitTimeout = &waitIndefinitely
	}

	ctx := context.Background()

	err := execute(ctx, *baseURL, *user, *password, *jobName, *jobParameters, *buildID, *waitTimeout, *waitPoolingInterval)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
