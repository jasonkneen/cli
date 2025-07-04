package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/agentuity/cli/internal/bundler"
	"github.com/agentuity/cli/internal/deployer"
	"github.com/agentuity/cli/internal/dev"
	"github.com/agentuity/cli/internal/envutil"
	"github.com/agentuity/cli/internal/errsystem"
	"github.com/agentuity/cli/internal/project"
	"github.com/agentuity/cli/internal/util"
	"github.com/agentuity/go-common/env"
	"github.com/agentuity/go-common/tui"
	"github.com/spf13/cobra"
)

var devCmd = &cobra.Command{
	Use:     "dev",
	Aliases: []string{"run"},
	Args:    cobra.NoArgs,
	Short:   "Run the development server",
	Long: `Run the development server for local testing and development.

This command starts a local development server that connects to the Agentuity Cloud
for live development and testing of your agents. It watches for file changes and
automatically rebuilds your project when changes are detected.

Flags:
  --dir            The directory to run the development server in

Examples:
  agentuity dev
  agentuity dev --dir /path/to/project`,
	Run: func(cmd *cobra.Command, args []string) {
		log := env.NewLogger(cmd)
		logLevel := env.LogLevel(cmd)
		apiUrl, appUrl, transportUrl := util.GetURLs(log)

		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		apiKey, userId := util.EnsureLoggedIn(ctx, log, cmd)
		theproject := project.EnsureProject(ctx, cmd)
		dir := theproject.Dir

		checkForUpgrade(ctx, log, false)

		if theproject.NewProject {
			var projectId string
			if theproject.Project.ProjectId != "" {
				projectId = theproject.Project.ProjectId
			}
			ShowNewProjectImport(ctx, log, cmd, theproject.APIURL, apiKey, projectId, theproject.Project, dir, false)
		}

		project, err := theproject.Project.GetProject(ctx, log, theproject.APIURL, apiKey, false, true)
		if err != nil {
			errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithUserMessage("Failed to validate project (%s). This is most likely due to the API key being invalid or the project has been deleted.\n\nYou can import this project using the following command:\n\n"+tui.Command("project import"), theproject.Project.ProjectId), errsystem.WithContextMessage(fmt.Sprintf("Failed to get project: %s", err))).ShowErrorAndExit()
		}

		var envfile *deployer.EnvFile

		envfile, project = envutil.ProcessEnvFiles(ctx, log, dir, theproject.Project, project, theproject.APIURL, apiKey, false, true)

		if envfile == nil {
			// we don't have an env file so we need to create one since this likely means you have cloned a new project
			filename := filepath.Join(dir, ".env")
			of, err := os.Create(filename)
			if err != nil {
				errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage("Failed to create .env file")).ShowErrorAndExit()
			}
			defer of.Close()
			for k, v := range project.Env {
				fmt.Fprintf(of, "%s=%s\n", k, v)
			}
			for k, v := range project.Secrets {
				fmt.Fprintf(of, "%s=%s\n", k, v)
			}
			of.Close()
			tui.ShowSuccess("Synchronized project to .env file: %s", tui.Muted(filename))
		}

		orgId := project.OrgId

		port, _ := cmd.Flags().GetInt("port")
		port, err = dev.FindAvailablePort(theproject, port)
		if err != nil {
			log.Fatal("failed to find available port: %s", err)
		}

		server, err := dev.New(dev.ServerArgs{
			Ctx:          ctx,
			Logger:       log,
			LogLevel:     logLevel,
			APIURL:       apiUrl,
			TransportURL: transportUrl,
			APIKey:       apiKey,
			OrgId:        orgId,
			Project:      theproject,
			Version:      Version,
			UserId:       userId,
			Port:         port,
		})
		if err != nil {
			log.Fatal("failed to create live dev connection: %s", err)
		}
		defer server.Close()

		processCtx := context.Background()
		var pid int32

		waitForConnection := func() {
			if err := server.Connect(); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				log.Error("failed to start live dev connection: %s", err)
				return
			}
		}

		tui.ShowSpinner("Connecting ...", waitForConnection)

		publicUrl := server.PublicURL(appUrl)
		consoleUrl := server.WebURL(appUrl)
		devModeUrl := fmt.Sprintf("http://127.0.0.1:%d", port)
		infoBox := server.GenerateInfoBox(publicUrl, consoleUrl, devModeUrl)
		fmt.Println(infoBox)

		projectServerCmd, err := dev.CreateRunProjectCmd(processCtx, log, theproject, server, dir, orgId, port, os.Stdout, os.Stderr)
		if err != nil {
			errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage("Failed to run project")).ShowErrorAndExit()
		}

		build := func(initial bool) bool {
			started := time.Now()
			var ok bool
			tui.ShowSpinner("Building project ...", func() {
				if err := bundler.Bundle(bundler.BundleContext{
					Context:    ctx,
					Logger:     log,
					ProjectDir: dir,
					Production: false,
					DevMode:    true,
					Writer:     os.Stdout,
				}); err != nil {
					if err == bundler.ErrBuildFailed {
						return
					}
					errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage(fmt.Sprintf("Failed to bundle project: %s", err))).ShowErrorAndExit()
				}
				ok = true
			})
			if ok && !initial {
				log.Info("✨ Built in %s", time.Since(started).Round(time.Millisecond))
			}
			return ok
		}

		runServer := func() {
			projectServerCmd, err = dev.CreateRunProjectCmd(processCtx, log, theproject, server, dir, orgId, port, os.Stdout, os.Stderr)
			if err != nil {
				errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage("Failed to run project")).ShowErrorAndExit()
			}
			if err := projectServerCmd.Start(); err != nil {
				errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage(fmt.Sprintf("Failed to start project: %s", err))).ShowErrorAndExit()
			}
			atomic.StoreInt32(&pid, int32(projectServerCmd.Process.Pid))
			// running = true
			log.Trace("restarted project server (pid: %d)", projectServerCmd.Process.Pid)
			log.Trace("waiting for project server to exit (pid: %d)", projectServerCmd.Process.Pid)
			if err := projectServerCmd.Wait(); err != nil {
				log.Error("project server (pid: %d) exited with error: %s", projectServerCmd.Process.Pid, err)
			}
			if projectServerCmd.ProcessState != nil {
				log.Debug("project server (pid: %d) exited with code %d", projectServerCmd.Process.Pid, projectServerCmd.ProcessState.ExitCode())
			} else {
				log.Debug("project server (pid: %d) exited", projectServerCmd.Process.Pid)
			}
		}

		// Initial build must exit if it fails
		if !build(true) {
			return
		}

		var restartingLock sync.Mutex

		restart := func() {
			// prevent multiple restarts from happening at once
			restartingLock.Lock()
			defer restartingLock.Unlock()
			dev.KillProjectServer(log, projectServerCmd, int(atomic.LoadInt32(&pid)))
			if build(false) {
				log.Trace("build ready")
				go runServer()
			}
		}

		rules := createProjectIgnoreRules(dir, theproject.Project, true)

		// Watch for changes
		watcher, err := dev.NewWatcher(log, dir, rules, func(path string) {
			log.Trace("%s has changed", path)
			restart()
		})
		if err != nil {
			errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage(fmt.Sprintf("Failed to start watcher: %s", err))).ShowErrorAndExit()
		}
		defer watcher.Close(log)

		initRun := func() {
			log.Trace("starting project server")
			if err := projectServerCmd.Start(); err != nil {
				errsystem.New(errsystem.ErrInvalidConfiguration, err, errsystem.WithContextMessage(fmt.Sprintf("Failed to start project: %s", err))).ShowErrorAndExit()
			}

			atomic.StoreInt32(&pid, int32(projectServerCmd.Process.Pid))
			log.Trace("started project server with pid: %d", projectServerCmd.Process.Pid)

			if err := server.HealthCheck(devModeUrl); err != nil {
				log.Error("failed to health check connection: %s", err)
				dev.KillProjectServer(log, projectServerCmd, projectServerCmd.Process.Pid)
				return
			}
		}

		tui.ShowSpinner("Starting Agents ...", initRun)

		log.Info("🚀 DevMode ready")

		teardown := func() {
			restartingLock.Lock()
			defer restartingLock.Unlock()
			watcher.Close(log)
			server.Close()
			if projectServerCmd != nil {
				dev.KillProjectServer(log, projectServerCmd, int(atomic.LoadInt32(&pid)))
				projectServerCmd.Wait()
			}
		}

		<-ctx.Done()

		fmt.Printf("\b\b\033[K") // remove the ^C

		teardown()

		log.Info("👋 See you next time!")
	},
}

func init() {
	rootCmd.AddCommand(devCmd)
	devCmd.Flags().StringP("dir", "d", ".", "The directory to run the development server in")
	devCmd.Flags().Int("port", 0, "The port to run the development server on (uses project default if not provided)")
}
