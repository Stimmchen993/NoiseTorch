// This file is part of the program "NoiseTorch-ng".
// Please see the LICENSE file for copyright information.

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noisetorch/pulseaudio"
)

const (
	loaded = iota
	unloaded
	inconsistent
)

const (
	pipewireWebRTCMicSource  = "nui_pw_mic_webrtc_src"
	pipewireWebRTCMicSink    = "nui_pw_mic_webrtc_sink"
	pipewireWebRTCMicRefSink = "nui_pw_mic_webrtc_ref_sink"
)

type ladspaDenoiserConfig struct {
	plugin  string
	label   string
	control string
}

// the ugly and (partially) repeated strings are unforunately difficult to avoid, as it's what pulse audio expects

func updateNoiseSupressorLoaded(ctx *ntcontext) {
	c := ctx.paClient
	upd, err := c.Updates()
	if err != nil {
		fmt.Printf("Error listening for updates: %v\n", err)
		return
	}

	for {
		ctx.noiseSupressorState, ctx.virtualDeviceInUse = supressorState(ctx)
		if !c.Connected() {
			break
		}

		<-upd
	}
}

func supressorState(ctx *ntcontext) (int, bool) {
	//perform some checks to see if it looks like the noise supressor is loaded
	c := ctx.paClient
	var inpLoaded, outLoaded, inputInc, outputInc bool
	var virtualDeviceInUse bool = false
	if ctx.config.FilterInput {
		if ctx.serverInfo.servertype == servertype_pipewire {
			module, ladspasource, err := findModule(c, "module-ladspa-source", "source_name='Filtered Microphone")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-ladspa-source: %v\n", err)
			}
			remapModule, remapsource, err := findModule(c, "module-remap-source", "source_name='Filtered Microphone")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-remap-source: %v\n", err)
			}
			webrtcModule, webrtcsource, err := findModule(c, "module-echo-cancel", "source_name="+pipewireWebRTCMicSource)
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-echo-cancel: %v\n", err)
			}
			virtualDeviceInUse = virtualDeviceInUse || (module.NUsed != 0) || (remapModule.NUsed != 0) || (webrtcModule.NUsed != 0)
			inpLoaded = ladspasource || remapsource
			inputInc = !inpLoaded && webrtcsource
		} else {
			_, nullsink, err := findModule(c, "module-null-sink", "sink_name=nui_mic_denoised_out")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-null-sink: %v\n", err)
			}
			_, ladspasink, err := findModule(c, "module-ladspa-sink", "sink_name=nui_mic_raw_in sink_master=nui_mic_denoised_out")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-ladspa-sink: %v\n", err)
			}
			_, loopback, err := findModule(c, "module-loopback", "sink=nui_mic_raw_in")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-loopback: %v\n", err)
			}
			module, remap, err := findModule(c, "module-remap-source", "master=nui_mic_denoised_out.monitor source_name=nui_mic_remap")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-remap-source: %v\n", err)
			}

			virtualDeviceInUse = virtualDeviceInUse || (module.NUsed != 0)

			if nullsink && ladspasink && loopback && remap {
				inpLoaded = true
			} else if nullsink || ladspasink || loopback || remap {
				inputInc = true
			}
		}
	} else {
		inpLoaded = true
	}

	if ctx.config.FilterOutput {
		if ctx.serverInfo.servertype == servertype_pipewire {
			module, ladspasink, err := findModule(c, "module-ladspa-sink", "sink_name='Filtered Headphones'")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for module-ladspa-sink: %v\n", err)
			}
			virtualDeviceInUse = virtualDeviceInUse || (module.NUsed != 0)
			outLoaded = ladspasink
			outputInc = false
		} else {
			_, out, err := findModule(c, "module-null-sink", "sink_name=nui_out_out_sink")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for output module-ladspa-sink: %v\n", err)
			}
			_, lad, err := findModule(c, "module-ladspa-sink", "sink_name=nui_out_ladspa")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for output module-ladspa-sink: %v\n", err)
			}
			_, loop, err := findModule(c, "module-loopback", "source=nui_out_out_sink.monitor")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for output module-ladspa-sink: %v\n", err)
			}
			module, outin, err := findModule(c, "module-null-sink", "sink_name=nui_out_in_sink")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for output module-ladspa-sink: %v\n", err)
			}
			virtualDeviceInUse = virtualDeviceInUse || (module.NUsed != 0)
			_, loop2, err := findModule(c, "module-loopback", "source=nui_out_in_sink.monitor")
			if err != nil {
				log.Printf("Couldn't fetch module list to check for output module-ladspa-sink: %v\n", err)
			}

			outLoaded = out && lad && loop && outin && loop2
			outputInc = out || lad || loop || outin || loop2
		}
	} else {
		outLoaded = true
	}

	if (inpLoaded || !ctx.config.FilterInput) && (outLoaded || !ctx.config.FilterOutput) && !inputInc {
		return loaded, virtualDeviceInUse
	}

	if (inpLoaded && ctx.config.FilterInput) || (outLoaded && ctx.config.FilterOutput) || inputInc || outputInc {
		return inconsistent, virtualDeviceInUse
	}

	return unloaded, virtualDeviceInUse
}

func loadSupressor(ctx *ntcontext, inp *device, out *device) error {
	if ctx.serverInfo.servertype == servertype_pulse {
		log.Printf("Querying pulse rlimit\n")
		pid, err := getPulsePid()
		if err != nil {
			return err
		}

		lim, err := getRlimit(pid)
		if err != nil {
			return err
		}
		log.Printf("Rlimit: %+v. Trying to remove.\n", lim)

		removeRlimit(pid)

		defer setRlimit(pid, &lim) // lowering RLIMIT doesn't require root

		newLim, err := getRlimit(pid)
		if err != nil {
			return err
		}
		log.Printf("Rlimit: %+v\n", newLim)
	}

	if inp.checked {
		if err := maybeApplyAutoAecProfile(ctx); err != nil {
			log.Printf("Couldn't auto-apply AEC profile: %v\n", err)
		}

		var err error
		if ctx.serverInfo.servertype == servertype_pipewire {
			err = loadPipeWireInput(ctx, inp)
		} else {
			err = loadPulseInput(ctx, inp)
		}
		if err != nil {
			log.Printf("Error loading input: %v\n", err)
			return err
		}
	}

	if out.checked {
		var err error
		if ctx.serverInfo.servertype == servertype_pipewire {
			err = loadPipeWireOutput(ctx, out)
		} else {
			err = loadPulseOutput(ctx, out)
		}
		if err != nil {
			log.Printf("Error loading output: %v\n", err)
			return err
		}
	}

	return nil
}

func loadModule(ctx *ntcontext, module, args string) (uint32, error) {
	idx, err := ctx.paClient.LoadModule(module, args)

	//14 = module initialisation failed
	if paErr, ok := err.(*pulseaudio.Error); ok && paErr.Code == 14 {
		resetUI(ctx)
		ctx.views.Push(makeErrorView(ctx, fmt.Sprintf("Could not load module '%s'. This is likely a problem with your system or distribution.", module)))
	}
	return idx, err
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func pipeWireWebRTCAecArgs(ctx *ntcontext) string {
	targetLevelDBFS := clampInt(-ctx.config.MicTargetLUFS, 12, 30)

	return fmt.Sprintf("analog_gain_control=%d digital_gain_control=%d noise_suppression=%d voice_detection=%d high_pass_filter=%d extended_filter=%d delay_agnostic=%d target_level_dbfs=%d",
		boolToInt(ctx.config.MicWebRTCAnalogGain),
		boolToInt(ctx.config.MicWebRTCAutoGain),
		boolToInt(ctx.config.MicWebRTCNoiseSuppress),
		boolToInt(ctx.config.MicWebRTCVoiceDetect),
		boolToInt(ctx.config.MicWebRTCHighPass),
		boolToInt(ctx.config.MicWebRTCExtended),
		boolToInt(ctx.config.MicWebRTCDelayAgnostic),
		targetLevelDBFS,
	)
}

func loadPipeWireInput(ctx *ntcontext, inp *device) error {
	log.Printf("Loading supressor for pipewire\n")

	stageSource := inp.ID
	if ctx.config.MicEnableWebRTC {
		refSinkIdx, err := loadModule(ctx, "module-null-sink",
			fmt.Sprintf("sink_name=%s rate=48000 channels=1 sink_properties=\"device.description='NoiseTorch WebRTC Ref'\"", pipewireWebRTCMicRefSink))
		if err != nil {
			return err
		}
		log.Printf("Loaded WebRTC reference sink as idx: %d\n", refSinkIdx)

		aecArgs := pipeWireWebRTCAecArgs(ctx)
		idx, err := loadModule(ctx, "module-echo-cancel",
			fmt.Sprintf("source_name=%s sink_name=%s source_master=%s sink_master=%s rate=48000 channels=1 aec_method=webrtc aec_args=\"%s\"",
				pipewireWebRTCMicSource, pipewireWebRTCMicSink, inp.ID, pipewireWebRTCMicRefSink, aecArgs))
		if err != nil {
			if unloadErr := unloadAllMatching(ctx.paClient, "module-null-sink", "sink_name="+pipewireWebRTCMicRefSink); unloadErr != nil {
				log.Printf("Couldn't clean up WebRTC reference sink after load failure: %v\n", unloadErr)
			}
			return err
		}
		log.Printf("Loaded module-echo-cancel as idx: %d\n", idx)
		stageSource = pipewireWebRTCMicSource
	}

	denoiser, err := resolveLadspaDenoiserConfig(ctx)
	if err != nil {
		return err
	}

	if ctx.config.MicEnableRNNoise {
		moduleArgs := fmt.Sprintf("source_name='Filtered Microphone for %s' master=%s rate=48000 channels=1 label=%s plugin=%s",
			inp.Name, stageSource, denoiser.label, denoiser.plugin)
		if denoiser.control != "" {
			moduleArgs += " control=" + denoiser.control
		}

		idx, err := loadModule(ctx, "module-ladspa-source",
			moduleArgs)
		if err != nil {
			return err
		}
		log.Printf("Loaded ladspa source as idx: %d\n", idx)
		if err := applyInputMicGain(ctx, inp.ID); err != nil {
			log.Printf("Couldn't apply input mic gain: %v\n", err)
		}
		return nil
	}

	idx, err := loadModule(ctx, "module-remap-source",
		fmt.Sprintf("master=%s source_name='Filtered Microphone for %s' source_properties=\"device.description='Filtered Microphone for %s'\"",
			stageSource, inp.Name, inp.Name))
	if err != nil {
		return err
	}
	log.Printf("Loaded remap source as idx: %d\n", idx)
	if err := applyInputMicGain(ctx, inp.ID); err != nil {
		log.Printf("Couldn't apply input mic gain: %v\n", err)
	}
	return nil
}

func loadPipeWireOutput(ctx *ntcontext, out *device) error {
	log.Printf("Loading supressor for pipewire\n")

	pluginPath := ctx.librnnoise
	if p, err := ensurePersistentRNNoisePlugin(ctx); err == nil {
		pluginPath = p
	} else {
		log.Printf("Couldn't persist rnnoise plugin for PipeWire mode, falling back to temporary path: %v\n", err)
	}

	idx, err := loadModule(ctx, "module-ladspa-sink",
		fmt.Sprintf("sink_name='Filtered Headphones' master=%s "+
			"rate=48000 channels=1 "+
			"label=nt-filter plugin=%s control=%d", out.ID, pluginPath, ctx.config.Threshold))

	if err != nil {
		return err
	}
	log.Printf("Loaded ladspa source as idx: %d\n", idx)
	return nil
}

func loadPulseInput(ctx *ntcontext, inp *device) error {
	log.Printf("Loading supressor for pulse\n")
	idx, err := loadModule(ctx, "module-null-sink", "sink_name=nui_mic_denoised_out rate=48000")
	if err != nil {
		return err
	}
	log.Printf("Loaded null sink as idx: %d\n", idx)

	idx, err = loadModule(ctx, "module-ladspa-sink",
		fmt.Sprintf("sink_name=nui_mic_raw_in sink_master=nui_mic_denoised_out "+
			"label=nt-filter plugin=%s control=%d", ctx.librnnoise, ctx.config.Threshold))
	if err != nil {
		return err
	}
	log.Printf("Loaded ladspa sink as idx: %d\n", idx)

	if inp.dynamicLatency {
		idx, err = loadModule(ctx, "module-loopback",
			fmt.Sprintf("source=%s sink=nui_mic_raw_in channels=1 latency_msec=1 source_dont_move=true sink_dont_move=true", inp.ID))
		if err != nil {
			return err
		}
		log.Printf("Loaded loopback as idx: %d\n", idx)
	} else {
		idx, err = loadModule(ctx, "module-loopback",
			fmt.Sprintf("source=%s sink=nui_mic_raw_in channels=1 latency_msec=50 source_dont_move=true sink_dont_move=true adjust_time=1", inp.ID))
		if err != nil {
			return err
		}
		log.Printf("Loaded fixed latency loopback as idx: %d\n", idx)
	}

	idx, err = loadModule(ctx, "module-remap-source", fmt.Sprintf(`master=nui_mic_denoised_out.monitor `+
		`source_name=nui_mic_remap source_properties="device.description='Filtered Microphone for %s'"`, inp.Name))
	if err != nil {
		return err
	}
	log.Printf("Loaded remap source as idx: %d\n", idx)
	return nil
}

func loadPulseOutput(ctx *ntcontext, out *device) error {
	_, err := loadModule(ctx, "module-null-sink", `sink_name=nui_out_out_sink`)
	if err != nil {
		return err
	}

	_, err = loadModule(ctx, "module-null-sink", `sink_name=nui_out_in_sink sink_properties="device.description='Filtered Headphones'"`)
	if err != nil {
		return err
	}

	_, err = loadModule(ctx, "module-ladspa-sink", fmt.Sprintf(`sink_name=nui_out_ladspa sink_master=nui_out_out_sink `+
		`label=nt-filter channels=1 plugin=%s control=%d rate=%d`,
		ctx.librnnoise, ctx.config.Threshold, 48000))
	if err != nil {
		return err
	}

	_, err = loadModule(ctx, "module-loopback",
		fmt.Sprintf("source=nui_out_out_sink.monitor sink=%s channels=2 latency_msec=50 source_dont_move=true sink_dont_move=true", out.ID))
	if err != nil {
		return err
	}

	_, err = loadModule(ctx, "module-loopback",
		fmt.Sprintf("source=nui_out_in_sink.monitor sink=nui_out_ladspa channels=1 latency_msec=50 source_dont_move=true sink_dont_move=true"))
	if err != nil {
		return err
	}
	return nil
}

func unloadSupressor(ctx *ntcontext) error {
	if ctx.serverInfo.servertype == servertype_pipewire {
		return unloadSupressorPipeWire(ctx)
	} else {
		return unloadSupressorPulse(ctx)
	}
}

func unloadSupressorPipeWire(ctx *ntcontext) error {
	log.Printf("Unloading modules for pipewire\n")
	c := ctx.paClient

	log.Printf("Searching for module-ladspa-source\n")
	if err := unloadAllMatching(c, "module-ladspa-source", "source_name='Filtered Microphone"); err != nil {
		return err
	}

	log.Printf("Searching for module-ladspa-sink\n")
	if err := unloadAllMatching(c, "module-ladspa-sink", "sink_name='Filtered Headphones'"); err != nil {
		return err
	}

	log.Printf("Searching for module-remap-source\n")
	if err := unloadAllMatching(c, "module-remap-source", "source_name='Filtered Microphone"); err != nil {
		return err
	}

	log.Printf("Searching for module-echo-cancel\n")
	if err := unloadAllMatching(c, "module-echo-cancel", "source_name="+pipewireWebRTCMicSource); err != nil {
		return err
	}

	log.Printf("Searching for WebRTC reference sink\n")
	if err := unloadAllMatching(c, "module-null-sink", "sink_name="+pipewireWebRTCMicRefSink); err != nil {
		return err
	}
	return nil
}

func unloadSupressorPulse(ctx *ntcontext) error {
	log.Printf("Unloading modules for pulseaudio\n")

	if pid, err := getPulsePid(); err == nil {
		if lim, err := getRlimit(pid); err == nil {
			log.Printf("Trying to remove rlimit. Limit is: %+v\n", lim)
			removeRlimit(pid)
			newLim, _ := getRlimit(pid)
			log.Printf("Rlimit: %+v\n", newLim)
			defer setRlimit(pid, &lim)
		}

	}

	log.Printf("Searching for null-sink\n")
	c := ctx.paClient
	m, found, err := findModule(c, "module-null-sink", "sink_name=nui_mic_denoised_out")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found null-sink at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for ladspa-sink\n")
	m, found, err = findModule(c, "module-ladspa-sink", "sink_name=nui_mic_raw_in sink_master=nui_mic_denoised_out")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found ladspa-sink at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for loopback\n")
	m, found, err = findModule(c, "module-loopback", "sink=nui_mic_raw_in")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found loopback at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for remap-source\n")
	m, found, err = findModule(c, "module-remap-source", "master=nui_mic_denoised_out.monitor source_name=nui_mic_remap")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found remap source at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for output module-null-sink\n")
	m, found, err = findModule(c, "module-null-sink", "sink_name=nui_out_out_sink")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found output null sink at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for output module-null-sink\n")
	m, found, err = findModule(c, "module-null-sink", "sink_name=nui_out_in_sink")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found output null sink at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for output module-ladspa-sink\n")
	m, found, err = findModule(c, "module-ladspa-sink", "sink_name=nui_out_ladspa")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found output ladspa sink at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for output module-loopback\n")
	m, found, err = findModule(c, "module-loopback", "source=nui_out_out_sink.monitor")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found output loopback at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	log.Printf("Searching for output module-loopback\n")
	m, found, err = findModule(c, "module-loopback", "source=nui_out_in_sink.monitor")
	if err != nil {
		return err
	}
	if found {
		log.Printf("Found output loopback at id [%d], sending unload command\n", m.Index)
		c.UnloadModule(m.Index)
	}

	return nil
}

// Finds a module by exactly matching the module name, and checking if the second string is a substring of the argument
func findModule(c *pulseaudio.Client, name string, argMatch string) (module pulseaudio.Module, found bool, err error) {
	lst, err := c.ModuleList()

	if err != nil {
		return pulseaudio.Module{}, false, err
	}
	for _, m := range lst {
		if m.Name == name && strings.Contains(m.Argument, argMatch) {
			return m, true, nil
		}
	}

	return pulseaudio.Module{}, false, nil
}

func unloadAllMatching(c *pulseaudio.Client, name string, argMatch string) error {
	for i := 0; i < 32; i++ {
		m, found, err := findModule(c, name, argMatch)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		log.Printf("Found %s at id [%d], sending unload command\n", name, m.Index)
		c.UnloadModule(m.Index)
	}
	return fmt.Errorf("failed to unload all matching modules for %s (%s)", name, argMatch)
}

func applyInputMicGain(ctx *ntcontext, inputSourceID string) error {
	if inputSourceID == "" {
		return fmt.Errorf("input name is empty")
	}
	gain := ctx.config.MicInputGainPercent
	if gain < 25 {
		gain = 25
	}
	if gain > 300 {
		gain = 300
	}
	cmd := exec.Command("pactl", "set-source-volume", inputSourceID, fmt.Sprintf("%d%%", gain))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("set-source-volume failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func maybeApplyAutoAecProfile(ctx *ntcontext) error {
	if ctx.serverInfo.servertype != servertype_pipewire || !ctx.config.MicAutoProfile || !ctx.config.MicEnableWebRTC {
		return nil
	}

	mode, err := detectAutoAecProfileMode()
	if err != nil {
		return err
	}
	switch mode {
	case "singing":
		applySingingAecOnlyPreset(ctx)
	case "voice":
		applyVoiceAecOnlyPreset(ctx)
	default:
		return nil
	}
	ctx.config.MicAutoProfileLastMode = mode
	writeConfig(ctx.config)
	log.Printf("Auto AEC profile applied: %s\n", mode)
	return nil
}

func detectAutoAecProfileMode() (string, error) {
	out, err := exec.Command("pactl", "list", "sink-inputs").Output()
	if err != nil {
		return "", err
	}

	voiceKeywords := []string{"webrtc voiceengine", "discord", "teams", "zoom", "slack", "skype"}
	var appNames []string
	var mediaRoles []string

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "application.name = ") {
			parts := strings.SplitN(trim, "=", 2)
			if len(parts) == 2 {
				appNames = append(appNames, strings.Trim(strings.TrimSpace(parts[1]), "\""))
			}
		}
		if strings.HasPrefix(trim, "media.role = ") {
			parts := strings.SplitN(trim, "=", 2)
			if len(parts) == 2 {
				mediaRoles = append(mediaRoles, strings.Trim(strings.TrimSpace(parts[1]), "\""))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	for _, role := range mediaRoles {
		r := strings.ToLower(role)
		if strings.Contains(r, "music") || strings.Contains(r, "movie") || strings.Contains(r, "video") {
			return "singing", nil
		}
	}

	if len(appNames) == 0 {
		return "voice", nil
	}

	for _, app := range appNames {
		a := strings.ToLower(app)
		isVoice := false
		for _, kw := range voiceKeywords {
			if strings.Contains(a, kw) {
				isVoice = true
				break
			}
		}
		if !isVoice {
			return "singing", nil
		}
	}

	return "voice", nil
}

func resolveLadspaDenoiserConfig(ctx *ntcontext) (ladspaDenoiserConfig, error) {
	if ctx.config.MicUseDeepFilterNet {
		if path, ok := findDeepFilterPluginPath(); ok {
			control := strings.TrimSpace(ctx.config.MicDeepFilterControl)
			return ladspaDenoiserConfig{
				plugin:  path,
				label:   "deep_filter_mono",
				control: control,
			}, nil
		}
		return ladspaDenoiserConfig{}, fmt.Errorf("DeepFilterNet selected but plugin not found. Install libdeep_filter_ladspa.so in ~/.local/lib/ladspa, /usr/lib/ladspa, or /usr/lib64/ladspa")
	}

	pluginPath := ctx.librnnoise
	if p, err := ensurePersistentRNNoisePlugin(ctx); err == nil {
		pluginPath = p
	} else {
		log.Printf("Couldn't persist rnnoise plugin for PipeWire mode, falling back to temporary path: %v\n", err)
	}

	return ladspaDenoiserConfig{
		plugin:  pluginPath,
		label:   "nt-filter",
		control: fmt.Sprintf("%d", ctx.config.Threshold),
	}, nil
}

func findDeepFilterPluginPath() (string, bool) {
	home := os.Getenv("HOME")
	candidates := []string{
		filepath.Join(home, ".local", "lib", "ladspa", "libdeep_filter_ladspa.so"),
		filepath.Join(home, ".ladspa", "libdeep_filter_ladspa.so"),
		"/usr/lib64/ladspa/libdeep_filter_ladspa.so",
		"/usr/lib/ladspa/libdeep_filter_ladspa.so",
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, true
		}
	}
	return "", false
}
