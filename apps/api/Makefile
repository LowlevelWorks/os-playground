test-integration:
	PLAYGROUND_OS_IMAGE=$(abspath ../../../my-os/build/os.iso) \
	PLAYGROUND_BOOT_MARKER='x86_64 Kernel' \
	go test -tags integration -race -count=1 -v ./internal/vm/localqemu/ -run 'TestRealQEMU'
