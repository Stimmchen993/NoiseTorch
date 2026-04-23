// This file is part of the program "NoiseTorch-ng".
// Please see the LICENSE file for copyright information.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

const persistentLadspaPluginName = "librnnoise_ladspa.so"

func ensurePersistentRNNoisePlugin(ctx *ntcontext) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't resolve home directory: %w", err)
	}
	targetDir := filepath.Join(home, ".local", "share", "noisetorch", "ladspa")
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return "", fmt.Errorf("couldn't create LADSPA directory: %w", err)
	}
	targetPath := filepath.Join(targetDir, persistentLadspaPluginName)

	in, err := os.ReadFile(ctx.librnnoise)
	if err != nil {
		return "", fmt.Errorf("couldn't read embedded RNNoise plugin: %w", err)
	}

	if current, err := os.ReadFile(targetPath); err == nil && bytes.Equal(current, in) {
		return targetPath, nil
	}

	tmp := targetPath + ".tmp"
	if err := os.WriteFile(tmp, in, 0644); err != nil {
		return "", fmt.Errorf("couldn't write RNNoise plugin: %w", err)
	}
	if err := os.Rename(tmp, targetPath); err != nil {
		return "", fmt.Errorf("couldn't finalize RNNoise plugin: %w", err)
	}
	return targetPath, nil
}
