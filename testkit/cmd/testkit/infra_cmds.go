package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuannm99/testkit/testkit/core/config"
	"github.com/tuannm99/testkit/testkit/core/infra"
)

func (g *globals) stack(out io.Writer) (*config.Project, *infra.Stack, error) {
	p, err := config.LoadProject(g.root)
	if err != nil {
		return nil, nil, err
	}
	var log io.Writer
	if g.verbose {
		log = os.Stderr
	}
	return p, infra.NewStack(p, infra.NewDocker(log), out), nil
}

// selectServices resolves --services names (or "all") to descriptors.
func selectServices(p *config.Project, names []string) ([]*config.Service, error) {
	if len(names) == 0 {
		return nil, nil
	}
	all, err := config.LoadServices(p.Abs(p.ServicesDir))
	if err != nil {
		return nil, err
	}
	if len(names) == 1 && names[0] == "all" {
		var out []*config.Service
		for _, k := range config.SortedKeys(all) {
			out = append(out, all[k])
		}
		return out, nil
	}
	var out []*config.Service
	for _, n := range names {
		s, ok := all[n]
		if !ok {
			return nil, fmt.Errorf("unknown service %q (known: %s)", n, strings.Join(config.SortedKeys(all), ", "))
		}
		out = append(out, s)
	}
	return out, nil
}

func newDoctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this host can run TestKit (docker, compose, privileges, ports, RAM, disk)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			_, st, err := g.stack(out)
			if err != nil {
				return err
			}
			rep := st.Doctor(cmd.Context())
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
			for _, c := range rep.Checks {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.Status, c.Detail)
				if c.Fix != "" && c.Status != infra.OK {
					fmt.Fprintf(tw, "\t\t-> %s\n", c.Fix)
				}
			}
			tw.Flush()
			if err := st.SaveDoctor(rep); err != nil {
				return err
			}
			if rep.Failed() {
				return exitErr{2, "doctor: at least one check failed"}
			}
			fmt.Fprintln(out, "doctor: host is ready")
			return nil
		},
	}
}

func newUpCmd(g *globals) *cobra.Command {
	var profiles, services []string
	var build bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start infrastructure profiles and wait until every container is healthy (idempotent)",
		Example: `  testkit up                                   # core,stores,mocks for every service
  testkit up --services order-worker           # only what order-worker declares
  testkit up --profile core,stores,mocks,observability,chaos`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			p, st, err := g.stack(out)
			if err != nil {
				return err
			}
			svcs, err := selectServices(p, services)
			if err != nil {
				return err
			}
			if caps, ok := st.LoadCapabilities(); ok && !caps.DockerSock && contains(profiles, "observability") {
				fmt.Fprintln(out, "WARN: docker.sock cannot be mounted on this host; skipping profile observability")
				profiles = without(profiles, "observability")
			}
			started := time.Now()
			state, err := st.Up(cmd.Context(), infra.UpOptions{Profiles: profiles, Services: svcs, Build: build, Timeout: timeout})
			if err != nil {
				return err
			}
			for _, s := range svcs {
				if err := st.EnsureServiceImages(cmd.Context(), s, build); err != nil {
					return fmt.Errorf("service %s image: %w", s.Name, err)
				}
			}
			fmt.Fprintf(out, "up: %d services healthy in %s: %s\n", len(state.Selection.Services),
				time.Since(started).Round(time.Second), strings.Join(state.Selection.Services, ", "))
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&profiles, "profile", []string{"core", "stores", "mocks"}, "profiles: "+strings.Join(infra.Profiles, ","))
	cmd.Flags().StringSliceVar(&services, "services", nil, "service descriptors whose stores/mocks to start (default: all of each profile)")
	cmd.Flags().BoolVar(&build, "build", false, "rebuild TestKit and service images")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "max wait for health checks")
	return cmd
}

func newDownCmd(g *globals) *cobra.Command {
	var verify bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove every container, volume and network of this TestKit project (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			_, st, err := g.stack(out)
			if err != nil {
				return err
			}
			left, err := st.Down(cmd.Context())
			if err != nil {
				return err
			}
			if !left.Empty() {
				return exitErr{3, fmt.Sprintf("down: leftovers: containers=%v volumes=%v networks=%v",
					left.Containers, left.Volumes, left.Networks)}
			}
			if verify {
				fmt.Fprintln(out, "down: verified clean (0 containers, 0 volumes, 0 networks)")
			} else {
				fmt.Fprintln(out, "down: clean")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&verify, "verify", true, "fail if any resource of the project is left behind")
	return cmd
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the infrastructure containers and their health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			_, st, err := g.stack(out)
			if err != nil {
				return err
			}
			run, err := st.Running(cmd.Context())
			if err != nil {
				return err
			}
			if len(run) == 0 {
				fmt.Fprintln(out, "nothing running")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tSTATUS")
			for _, k := range config.SortedKeys(run) {
				fmt.Fprintf(tw, "%s\t%s\n", k, run[k])
			}
			return tw.Flush()
		},
	}
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func without(l []string, v string) []string {
	var out []string
	for _, x := range l {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
