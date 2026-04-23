// This file is part of the program "NoiseTorch-ng".
// Please see the LICENSE file for copyright information.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	pipewireNativeConfigName = "90-noisetorch.conf"

	pipewireNativeInputNodeName  = "noisetorch.filtered.input"
	pipewireNativeOutputNodeName = "noisetorch.filtered.output"

	persistentLadspaPluginName = "librnnoise_ladspa.so"
)

func loadPipeWireNative(ctx *ntcontext, inp *device, out *device) error {
	if !inp.checked && !out.checked {
		return errors.New("nothing selected for native PipeWire load")
	}

	filterChainDir, err := pipewireConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filterChainDir, 0700); err != nil {
		return fmt.Errorf("couldn't create pipewire filter-chain directory: %w", err)
	}

	pluginPath, err := ensurePersistentRNNoisePlugin(ctx)
	if err != nil {
		return err
	}
	if err := ensurePipeWireLadspaPath(filepath.Dir(pluginPath)); err != nil {
		return err
	}

	conf := buildPipeWireFilterConf(pluginPath, ctx.config.Threshold, inp, out)
	confPath := filepath.Join(filterChainDir, pipewireNativeConfigName)
	if err := os.WriteFile(confPath, []byte(conf), 0644); err != nil {
		return fmt.Errorf("couldn't write native PipeWire config: %w", err)
	}

	success := false
	defer func() {
		if success {
			return
		}
		if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Couldn't roll back native PipeWire config: %v\n", err)
		}
		_ = restartPipeWireServices()
	}()

	if err := restartPipeWireServices(); err != nil {
		return err
	}
	if inp.checked && !waitForPipeWireNode(pipewireNativeInputNodeName, 10*time.Second) {
		return errors.New("native PipeWire input node did not appear after restart")
	}
	if out.checked && !waitForPipeWireNode(pipewireNativeOutputNodeName, 10*time.Second) {
		return errors.New("native PipeWire output node did not appear after restart")
	}
	success = true
	return nil
}

func unloadPipeWireNative() (bool, error) {
	filterChainDir, err := pipewireConfigDir()
	if err != nil {
		return false, err
	}
	confPath := filepath.Join(filterChainDir, pipewireNativeConfigName)

	ok, err := exists(confPath)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := os.Remove(confPath); err != nil {
		return false, fmt.Errorf("couldn't remove native PipeWire config: %w", err)
	}

	return true, restartPipeWireServices()
}

func pipeWireNativeConfigured() bool {
	filterChainDir, err := pipewireConfigDir()
	if err != nil {
		return false
	}
	confPath := filepath.Join(filterChainDir, pipewireNativeConfigName)
	ok, err := exists(confPath)
	return err == nil && ok
}

func pipeWireNativeHasInput() bool {
	return pipeWireNativeConfigContains(pipewireNativeInputNodeName)
}

func pipeWireNativeHasOutput() bool {
	return pipeWireNativeConfigContains(pipewireNativeOutputNodeName)
}

func pipeWireNativeConfigContains(text string) bool {
	filterChainDir, err := pipewireConfigDir()
	if err != nil {
		return false
	}
	confPath := filepath.Join(filterChainDir, pipewireNativeConfigName)
	buf, err := os.ReadFile(confPath)
	if err != nil {
		return false
	}
	return strings.Contains(string(buf), text)
}

func pipewireConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "pipewire", "pipewire.conf.d"), nil
}

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

func buildPipeWireFilterConf(pluginPathNoExt string, threshold int, inp *device, out *device) string {
	var modules []string

	if inp.checked {
		description := fmt.Sprintf("Filtered Microphone for %s", inp.Name)
		modules = append(modules, fmt.Sprintf(`
    { name = libpipewire-module-filter-chain
        flags = [ nofail ]
        args = {
            node.description = %q
            media.name = %q
            filter.graph = {
                nodes = [
                    {
                        type = ladspa
                        name = rnnoise
                        plugin = %q
                        label = nt-filter
                        control = { 0 %0.1f }
                    }
                ]
            }
            audio.channels = 1
            audio.position = [ MONO ]
            capture.props = {
                node.name = "noisetorch.capture.input"
                node.passive = true
                target.object = %q
            }
            playback.props = {
                node.name = %q
                media.class = Audio/Source
            }
        }
    }`, description, description, pluginPathNoExt, float64(threshold), inp.ID, pipewireNativeInputNodeName))
	}

	if out.checked {
		modules = append(modules, fmt.Sprintf(`
    { name = libpipewire-module-filter-chain
        flags = [ nofail ]
        args = {
            node.description = "Filtered Headphones"
            media.name = "Filtered Headphones"
            filter.graph = {
                nodes = [
                    {
                        type = ladspa
                        name = rnnoise
                        plugin = %q
                        label = nt-filter
                        control = { 0 %0.1f }
                    }
                ]
            }
            audio.channels = 1
            audio.position = [ MONO ]
            capture.props = {
                node.name = "noisetorch.capture.output"
                node.description = "Filtered Headphones"
                media.class = Audio/Sink
            }
            playback.props = {
                node.name = %q
                node.passive = true
                target.object = %q
            }
        }
    }`, pluginPathNoExt, float64(threshold), pipewireNativeOutputNodeName, out.ID))
	}

	return "# Auto-generated by NoiseTorch. Changes may be overwritten.\ncontext.modules = [" +
		strings.Join(modules, "\n") + "\n]\n"
}

func restartPipeWireServices() error {
	cmd := exec.Command("systemctl", "--user", "restart", "pipewire.service", "pipewire-pulse.service", "wireplumber.service")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't restart PipeWire services: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensurePipeWireLadspaPath(extraDir string) error {
	existing := ""
	show := exec.Command("systemctl", "--user", "show-environment")
	if out, err := show.Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "LADSPA_PATH=") {
				existing = strings.TrimPrefix(line, "LADSPA_PATH=")
				break
			}
		}
	}

	search := []string{
		extraDir,
		"/usr/lib64/ladspa",
		"/usr/lib/ladspa",
		"/usr/lib",
	}
	if existing != "" {
		for _, part := range strings.Split(existing, ":") {
			if part != "" {
				search = append(search, part)
			}
		}
	}

	seen := make(map[string]struct{})
	filtered := make([]string, 0, len(search))
	for _, path := range search {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		filtered = append(filtered, path)
	}

	val := strings.Join(filtered, ":")
	cmd := exec.Command("systemctl", "--user", "set-environment", "LADSPA_PATH="+val)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("couldn't set LADSPA_PATH for user services: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func pipeWireNodeExists(nodeName string) bool {
	cmd := exec.Command("pw-dump")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("Couldn't inspect PipeWire graph: %v\n", err)
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf(`"node.name": "%s"`, nodeName))
}

func waitForPipeWireNode(nodeName string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pipeWireNodeExists(nodeName) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}
