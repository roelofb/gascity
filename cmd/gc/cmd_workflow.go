package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/workflow"
	"github.com/spf13/cobra"
)

func newWorkflowCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Run explicit graph-first workflow control beads",
	}
	cmd.AddCommand(newWorkflowControlCmd(stdout, stderr))
	return cmd
}

func newWorkflowControlCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "control <bead-id>",
		Short: "Execute a graph.v2 control bead in the current city",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := runWorkflowControl(args[0], stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "gc workflow control: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func runWorkflowControl(beadID string, stdout, _ io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		return err
	}

	readDoltPort(cityPath)
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return fmt.Errorf("opening workflow store: %w", err)
	}

	bead, err := store.Get(beadID)
	if err != nil {
		return fmt.Errorf("loading bead %s: %w", beadID, err)
	}

	result, err := workflow.ProcessControl(store, bead)
	if err != nil {
		return err
	}
	if result.Processed {
		fmt.Fprintf(stdout, "workflow control: bead=%s action=%s skipped=%d\n", beadID, result.Action, result.Skipped) //nolint:errcheck
	}
	return nil
}
