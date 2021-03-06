// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/syzkaller/fileutil"
	"github.com/google/syzkaller/sys"
	"github.com/google/syzkaller/vm"
)

type Config struct {
	Http    string
	Workdir string
	Vmlinux string
	Kernel  string // e.g. arch/x86/boot/bzImage
	Cmdline string // kernel command line
	Image   string // linux image for VMs
	Cpu     int    // number of VM CPUs
	Mem     int    // amount of VM memory in MBs
	Sshkey  string // root ssh key for the image
	Port    int    // VM ssh port to use
	Bin     string // qemu/lkvm binary name
	Debug   bool   // dump all VM output to console
	Output  string // one of stdout/dmesg/file (useful only for local VM)

	Syzkaller string // path to syzkaller checkout (syz-manager will look for binaries in bin subdir)
	Type      string // VM type (qemu, kvm, local)
	Count     int    // number of VMs
	Procs     int    // number of parallel processes inside of every VM

	Sandbox string // type of sandbox to use during fuzzing:
	// "none": don't do anything special (has false positives, e.g. due to killing init)
	// "setuid": impersonate into user nobody (65534), default
	// "namespace": create a new namespace for fuzzer using CLONE_NEWNS/CLONE_NEWNET/CLONE_NEWPID/etc,
	//	requires building kernel with CONFIG_NAMESPACES, CONFIG_UTS_NS, CONFIG_USER_NS, CONFIG_PID_NS and CONFIG_NET_NS.

	Cover bool // use kcov coverage (default: true)
	Leak  bool // do memory leak checking

	ConsoleDev string // console device for adb vm

	Enable_Syscalls  []string
	Disable_Syscalls []string
	Suppressions     []string
}

func Parse(filename string) (*Config, map[int]bool, []*regexp.Regexp, error) {
	if filename == "" {
		return nil, nil, nil, fmt.Errorf("supply config in -config flag")
	}
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read config file: %v", err)
	}
	return parse(data)
}

func parse(data []byte) (*Config, map[int]bool, []*regexp.Regexp, error) {
	unknown, err := checkUnknownFields(data)
	if err != nil {
		return nil, nil, nil, err
	}
	if unknown != "" {
		return nil, nil, nil, fmt.Errorf("unknown field '%v' in config", unknown)
	}
	cfg := new(Config)
	cfg.Cover = true
	cfg.Sandbox = "setuid"
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse config file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Syzkaller, "bin/syz-fuzzer")); err != nil {
		return nil, nil, nil, fmt.Errorf("bad config syzkaller param: can't find bin/syz-fuzzer")
	}
	if _, err := os.Stat(filepath.Join(cfg.Syzkaller, "bin/syz-executor")); err != nil {
		return nil, nil, nil, fmt.Errorf("bad config syzkaller param: can't find bin/syz-executor")
	}
	if cfg.Http == "" {
		return nil, nil, nil, fmt.Errorf("config param http is empty")
	}
	if cfg.Workdir == "" {
		return nil, nil, nil, fmt.Errorf("config param workdir is empty")
	}
	if cfg.Vmlinux == "" {
		return nil, nil, nil, fmt.Errorf("config param vmlinux is empty")
	}
	if cfg.Type == "" {
		return nil, nil, nil, fmt.Errorf("config param type is empty")
	}
	if cfg.Count <= 0 || cfg.Count > 1000 {
		return nil, nil, nil, fmt.Errorf("invalid config param count: %v, want (1, 1000]", cfg.Count)
	}
	if cfg.Procs <= 0 {
		cfg.Procs = 1
	}
	if cfg.Output == "" {
		if cfg.Type == "local" {
			cfg.Output = "none"
		} else {
			cfg.Output = "stdout"
		}
	}
	switch cfg.Output {
	case "none", "stdout", "dmesg", "file":
	default:
		return nil, nil, nil, fmt.Errorf("config param output must contain one of none/stdout/dmesg/file")
	}
	switch cfg.Sandbox {
	case "none", "setuid", "namespace":
	default:
		return nil, nil, nil, fmt.Errorf("config param sandbox must contain one of none/setuid/namespace")
	}

	syscalls, err := parseSyscalls(cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	suppressions, err := parseSuppressions(cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	return cfg, syscalls, suppressions, nil
}

func parseSyscalls(cfg *Config) (map[int]bool, error) {
	match := func(call *sys.Call, str string) bool {
		if str == call.CallName || str == call.Name {
			return true
		}
		if len(str) > 1 && str[len(str)-1] == '*' && strings.HasPrefix(call.Name, str[:len(str)-1]) {
			return true
		}
		return false
	}

	syscalls := make(map[int]bool)
	if len(cfg.Enable_Syscalls) != 0 {
		for _, c := range cfg.Enable_Syscalls {
			n := 0
			for _, call := range sys.Calls {
				if match(call, c) {
					syscalls[call.ID] = true
					n++
				}
			}
			if n == 0 {
				return nil, fmt.Errorf("unknown enabled syscall: %v", c)
			}
		}
	} else {
		for _, call := range sys.Calls {
			syscalls[call.ID] = true
		}
	}
	for _, c := range cfg.Disable_Syscalls {
		n := 0
		for _, call := range sys.Calls {
			if match(call, c) {
				delete(syscalls, call.ID)
				n++
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("unknown disabled syscall: %v", c)
		}
	}
	// They will be generated anyway.
	syscalls[sys.CallMap["mmap"].ID] = true
	syscalls[sys.CallMap["clock_gettime"].ID] = true

	return syscalls, nil
}

func parseSuppressions(cfg *Config) ([]*regexp.Regexp, error) {
	// Add some builtin suppressions.
	supp := append(cfg.Suppressions, []string{
		"panic: failed to start executor binary",
		"panic: executor failed: pthread_create failed",
		"panic: failed to create temp dir",
		"fatal error: runtime: out of memory",
		"Out of memory: Kill process .* \\(syz-fuzzer\\)",
		//"WARNING: KASAN doesn't support memory hot-add",
		//"INFO: lockdep is turned off", // printed by some sysrq that dumps scheduler state
	}...)
	var suppressions []*regexp.Regexp
	for _, s := range supp {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("failed to compile suppression '%v': %v", s, err)
		}
		suppressions = append(suppressions, re)
	}

	return suppressions, nil
}

func CreateVMConfig(cfg *Config) (*vm.Config, error) {
	workdir, index, err := fileutil.ProcessTempDir(cfg.Workdir)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance temp dir: %v", err)
	}
	vmCfg := &vm.Config{
		Name:       fmt.Sprintf("%v-%v", cfg.Type, index),
		Index:      index,
		Workdir:    workdir,
		Bin:        cfg.Bin,
		Kernel:     cfg.Kernel,
		Cmdline:    cfg.Cmdline,
		Image:      cfg.Image,
		Sshkey:     cfg.Sshkey,
		Executor:   filepath.Join(cfg.Syzkaller, "bin", "syz-executor"),
		ConsoleDev: cfg.ConsoleDev,
		Cpu:        cfg.Cpu,
		Mem:        cfg.Mem,
		Debug:      cfg.Debug,
	}
	return vmCfg, nil
}

func checkUnknownFields(data []byte) (string, error) {
	// While https://github.com/golang/go/issues/15314 is not resolved
	// we don't have a better way than to enumerate all known fields.
	var fields = []string{
		"Http",
		"Workdir",
		"Vmlinux",
		"Kernel",
		"Cmdline",
		"Image",
		"Cpu",
		"Mem",
		"Sshkey",
		"Port",
		"Bin",
		"Debug",
		"Output",
		"Syzkaller",
		"Type",
		"Count",
		"Procs",
		"Cover",
		"Sandbox",
		"Leak",
		"ConsoleDev",
		"Enable_Syscalls",
		"Disable_Syscalls",
		"Suppressions",
	}
	f := make(map[string]interface{})
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("failed to parse config file: %v", err)
	}
	for k := range f {
		ok := false
		for _, k1 := range fields {
			if strings.ToLower(k) == strings.ToLower(k1) {
				ok = true
				break
			}
		}
		if !ok {
			return k, nil
		}
	}
	return "", nil
}
