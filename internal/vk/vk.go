//go:build linux

// Package vk è un binding minimo di Vulkan per il calcolo sulla GPU, in Go
// puro: libvulkan.so.1 si carica a runtime con purego (dlopen, senza cgo),
// quindi il binario parte anche dove Vulkan manca e chi lo usa può ripiegare
// sulla CPU.
//
// Copre solo ciò che serve a eseguire una rete neurale già compilata:
// buffer (memoria della GPU e memoria condivisa con l'host), pipeline di
// compute con un layout unico (due storage buffer e 128 byte di push
// constant), command buffer registrati una volta e rieseguiti, timestamp.
//
// Le strutture Go qui sotto replicano byte per byte quelle C (amd64/arm64):
// Go allinea i campi come il C, i commenti danno gli offset.
package vk

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// PushSize sono i byte di push constant di ogni pipeline.
const PushSize = 128

const (
	stApplicationInfo          = 0
	stInstanceCreateInfo       = 1
	stDeviceQueueCreateInfo    = 2
	stDeviceCreateInfo         = 3
	stSubmitInfo               = 4
	stMemoryAllocateInfo       = 5
	stFenceCreateInfo          = 8
	stQueryPoolCreateInfo      = 11
	stBufferCreateInfo         = 12
	stShaderModuleCreateInfo   = 16
	stPipelineShaderStage      = 18
	stComputePipelineCreate    = 29
	stPipelineLayoutCreate     = 30
	stDescriptorSetLayout      = 32
	stDescriptorPoolCreate     = 33
	stDescriptorSetAllocate    = 34
	stWriteDescriptorSet       = 35
	stCommandPoolCreate        = 39
	stCommandBufferAllocate    = 40
	stCommandBufferBegin       = 42
	stMemoryBarrier            = 46
	stPhysicalDeviceProps2     = 1000059001
	stPhysicalDeviceDriverProp = 1000196000

	descStorageBuffer = 7
	stageCompute      = 0x20

	bufUsageTransferSrc = 0x1
	bufUsageTransferDst = 0x2
	bufUsageStorage     = 0x20

	memDeviceLocal  = 0x1
	memHostVisible  = 0x2
	memHostCoherent = 0x4
	memHostCached   = 0x8

	pipeStageTop      = 0x1
	pipeStageCompute  = 0x800
	pipeStageTransfer = 0x1000
	pipeStageBottom   = 0x2000
	pipeStageHost     = 0x4000

	accShaderRead    = 0x20
	accShaderWrite   = 0x40
	accTransferRead  = 0x800
	accTransferWrite = 0x1000
	accHostRead      = 0x2000
	accHostWrite     = 0x4000

	queueCompute = 0x2

	queryTimestamp = 2
	queryResult64  = 0x1
	queryWait      = 0x2

	vkSuccess = 0
	vkTimeout = 2
)

var resultNames = map[int32]string{
	-1: "VK_ERROR_OUT_OF_HOST_MEMORY", -2: "VK_ERROR_OUT_OF_DEVICE_MEMORY", -3: "VK_ERROR_INITIALIZATION_FAILED",
	-4: "VK_ERROR_DEVICE_LOST", -5: "VK_ERROR_MEMORY_MAP_FAILED", -6: "VK_ERROR_LAYER_NOT_PRESENT",
	-7: "VK_ERROR_EXTENSION_NOT_PRESENT", -8: "VK_ERROR_FEATURE_NOT_PRESENT", -9: "VK_ERROR_INCOMPATIBLE_DRIVER",
	-10: "VK_ERROR_TOO_MANY_OBJECTS", -13: "VK_ERROR_UNKNOWN", 2: "VK_TIMEOUT",
}

// Error è un VkResult diverso da VK_SUCCESS.
type Error struct {
	Call   string
	Result int32
}

func (e *Error) Error() string {
	n := resultNames[e.Result]
	if n == "" {
		n = fmt.Sprintf("VkResult %d", e.Result)
	}
	return "vulkan: " + e.Call + ": " + n
}

func check(call string, r int32) error {
	if r == vkSuccess {
		return nil
	}
	return &Error{call, r}
}

// --- strutture (offset amd64/arm64) -----------------------------------------

type applicationInfo struct {
	sType              uint32         // 0
	pNext              unsafe.Pointer // 8
	pApplicationName   *byte          // 16
	applicationVersion uint32         // 24
	pEngineName        *byte          // 32
	engineVersion      uint32         // 40
	apiVersion         uint32         // 44
}

type instanceCreateInfo struct {
	sType                   uint32         // 0
	pNext                   unsafe.Pointer // 8
	flags                   uint32         // 16
	pApplicationInfo        *applicationInfo
	enabledLayerCount       uint32 // 32
	ppEnabledLayerNames     **byte // 40
	enabledExtensionCount   uint32 // 48
	ppEnabledExtensionNames **byte // 56
}

type deviceQueueCreateInfo struct {
	sType            uint32
	pNext            unsafe.Pointer
	flags            uint32
	queueFamilyIndex uint32
	queueCount       uint32
	pQueuePriorities *float32
}

type deviceCreateInfo struct {
	sType                   uint32
	pNext                   unsafe.Pointer
	flags                   uint32
	queueCreateInfoCount    uint32
	pQueueCreateInfos       *deviceQueueCreateInfo
	enabledLayerCount       uint32
	ppEnabledLayerNames     **byte
	enabledExtensionCount   uint32
	ppEnabledExtensionNames **byte
	pEnabledFeatures        unsafe.Pointer
}

type queueFamilyProperties struct {
	queueFlags         uint32
	queueCount         uint32
	timestampValidBits uint32
	granularity        [3]uint32
}

type memoryType struct {
	propertyFlags uint32
	heapIndex     uint32
}

type memoryHeap struct {
	size  uint64
	flags uint32
	_     uint32
}

type memoryProperties struct {
	memoryTypeCount uint32
	memoryTypes     [32]memoryType
	memoryHeapCount uint32
	memoryHeaps     [16]memoryHeap
}

type physicalDeviceProperties2 struct {
	sType uint32
	pNext unsafe.Pointer
	props [824]byte // VkPhysicalDeviceProperties
}

type driverProperties struct {
	sType       uint32
	pNext       unsafe.Pointer
	driverID    uint32
	driverName  [256]byte
	driverInfo  [256]byte
	conformance [4]byte
}

type bufferCreateInfo struct {
	sType                 uint32
	pNext                 unsafe.Pointer
	flags                 uint32
	size                  uint64
	usage                 uint32
	sharingMode           uint32
	queueFamilyIndexCount uint32
	pQueueFamilyIndices   *uint32
}

type memoryRequirements struct {
	size           uint64
	alignment      uint64
	memoryTypeBits uint32
}

type memoryAllocateInfo struct {
	sType           uint32
	pNext           unsafe.Pointer
	allocationSize  uint64
	memoryTypeIndex uint32
}

type shaderModuleCreateInfo struct {
	sType    uint32
	pNext    unsafe.Pointer
	flags    uint32
	codeSize uintptr
	pCode    *uint32
}

type descriptorSetLayoutBinding struct {
	binding            uint32
	descriptorType     uint32
	descriptorCount    uint32
	stageFlags         uint32
	pImmutableSamplers unsafe.Pointer
}

type descriptorSetLayoutCreateInfo struct {
	sType        uint32
	pNext        unsafe.Pointer
	flags        uint32
	bindingCount uint32
	pBindings    *descriptorSetLayoutBinding
}

type pushConstantRange struct {
	stageFlags uint32
	offset     uint32
	size       uint32
}

type pipelineLayoutCreateInfo struct {
	sType                  uint32
	pNext                  unsafe.Pointer
	flags                  uint32
	setLayoutCount         uint32
	pSetLayouts            *uint64
	pushConstantRangeCount uint32
	pPushConstantRanges    *pushConstantRange
}

type pipelineShaderStageCreateInfo struct {
	sType               uint32
	pNext               unsafe.Pointer
	flags               uint32
	stage               uint32
	module              uint64
	pName               *byte
	pSpecializationInfo unsafe.Pointer
}

type computePipelineCreateInfo struct {
	sType              uint32
	pNext              unsafe.Pointer
	flags              uint32
	stage              pipelineShaderStageCreateInfo // 24
	layout             uint64                        // 72
	basePipelineHandle uint64
	basePipelineIndex  int32
}

type descriptorPoolSize struct {
	typ   uint32
	count uint32
}

type descriptorPoolCreateInfo struct {
	sType         uint32
	pNext         unsafe.Pointer
	flags         uint32
	maxSets       uint32
	poolSizeCount uint32
	pPoolSizes    *descriptorPoolSize
}

type descriptorSetAllocateInfo struct {
	sType              uint32
	pNext              unsafe.Pointer
	descriptorPool     uint64
	descriptorSetCount uint32
	pSetLayouts        *uint64
}

type descriptorBufferInfo struct {
	buffer uint64
	offset uint64
	rng    uint64
}

type writeDescriptorSet struct {
	sType            uint32
	pNext            unsafe.Pointer
	dstSet           uint64
	dstBinding       uint32
	dstArrayElement  uint32
	descriptorCount  uint32
	descriptorType   uint32
	pImageInfo       unsafe.Pointer
	pBufferInfo      *descriptorBufferInfo
	pTexelBufferView unsafe.Pointer
}

type commandPoolCreateInfo struct {
	sType            uint32
	pNext            unsafe.Pointer
	flags            uint32
	queueFamilyIndex uint32
}

type commandBufferAllocateInfo struct {
	sType              uint32
	pNext              unsafe.Pointer
	commandPool        uint64
	level              uint32
	commandBufferCount uint32
}

type commandBufferBeginInfo struct {
	sType            uint32
	pNext            unsafe.Pointer
	flags            uint32
	pInheritanceInfo unsafe.Pointer
}

type memoryBarrier struct {
	sType         uint32
	pNext         unsafe.Pointer
	srcAccessMask uint32
	dstAccessMask uint32
}

type bufferCopy struct {
	srcOffset uint64
	dstOffset uint64
	size      uint64
}

type submitInfo struct {
	sType                uint32
	pNext                unsafe.Pointer
	waitSemaphoreCount   uint32
	pWaitSemaphores      unsafe.Pointer
	pWaitDstStageMask    unsafe.Pointer
	commandBufferCount   uint32
	pCommandBuffers      *uintptr
	signalSemaphoreCount uint32
	pSignalSemaphores    unsafe.Pointer
}

type fenceCreateInfo struct {
	sType uint32
	pNext unsafe.Pointer
	flags uint32
}

type queryPoolCreateInfo struct {
	sType              uint32
	pNext              unsafe.Pointer
	flags              uint32
	queryType          uint32
	queryCount         uint32
	pipelineStatistics uint32
}

// --- funzioni ---------------------------------------------------------------

type funcs struct {
	getInstanceProcAddr uintptr
	createInstance      uintptr

	destroyInstance                     uintptr
	enumeratePhysicalDevices            uintptr
	getPhysicalDeviceProperties2        uintptr
	getPhysicalDeviceQueueFamilyProps   uintptr
	getPhysicalDeviceMemoryProperties   uintptr
	createDevice                        uintptr
	getDeviceProcAddr                   uintptr
	enumerateInstanceLayerProperties    uintptr
	enumerateDeviceExtensionsProperties uintptr

	destroyDevice               uintptr
	getDeviceQueue              uintptr
	createBuffer                uintptr
	destroyBuffer               uintptr
	getBufferMemoryRequirements uintptr
	allocateMemory              uintptr
	freeMemory                  uintptr
	bindBufferMemory            uintptr
	mapMemory                   uintptr
	unmapMemory                 uintptr
	createShaderModule          uintptr
	destroyShaderModule         uintptr
	createDescriptorSetLayout   uintptr
	destroyDescriptorSetLayout  uintptr
	createPipelineLayout        uintptr
	destroyPipelineLayout       uintptr
	createComputePipelines      uintptr
	destroyPipeline             uintptr
	createDescriptorPool        uintptr
	destroyDescriptorPool       uintptr
	allocateDescriptorSets      uintptr
	updateDescriptorSets        uintptr
	createCommandPool           uintptr
	destroyCommandPool          uintptr
	allocateCommandBuffers      uintptr
	freeCommandBuffers          uintptr
	beginCommandBuffer          uintptr
	endCommandBuffer            uintptr
	cmdBindPipeline             uintptr
	cmdBindDescriptorSets       uintptr
	cmdPushConstants            uintptr
	cmdDispatch                 uintptr
	cmdPipelineBarrier          uintptr
	cmdCopyBuffer               uintptr
	cmdFillBuffer               uintptr
	cmdWriteTimestamp           uintptr
	cmdResetQueryPool           uintptr
	queueSubmit                 uintptr
	queueWaitIdle               uintptr
	createFence                 uintptr
	destroyFence                uintptr
	waitForFences               uintptr
	resetFences                 uintptr
	createQueryPool             uintptr
	destroyQueryPool            uintptr
	getQueryPoolResults         uintptr
	deviceWaitIdle              uintptr
}

// call esegue una funzione C; i puntatori convertiti in uintptr
// direttamente nella lista degli argomenti (uintptr(unsafe.Pointer(&x)))
// finiscono sullo heap e restano vivi per tutta la chiamata. La conversione
// va scritta lì: fatta in una funzione di appoggio, il compilatore non la
// vede e la variabile può restare sullo stack, che può spostarsi prima
// della chiamata.
//
//go:uintptrescapes
func call(fn uintptr, args ...uintptr) int32 {
	r, _, _ := purego.SyscallN(fn, args...)
	return int32(r)
}

func cstr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

func goString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// --- Device -------------------------------------------------------------------

// Info descrive la GPU scelta.
type Info struct {
	Name       string
	Type       string // discreta, integrata, virtuale, CPU
	VendorID   uint32
	DeviceID   uint32
	API        string // versione Vulkan del device
	Driver     string // es. radv
	DriverInfo string // es. Mesa 25.x
	// Nanosecondi per tick dei timestamp; 0 se la coda non li supporta.
	TimestampNs    float64
	SharedMem      int    // byte di memoria condivisa per workgroup
	MaxInvocations int    // thread per workgroup
	DeviceMemory   uint64 // byte della memoria locale (heap più grande)
	QueueFamily    int
}

// Device è un device Vulkan con una coda di calcolo. I metodi non sono
// concorrenti salvo dove indicato.
type Device struct {
	Info Info

	lib      uintptr
	fn       funcs
	instance uintptr
	physical uintptr
	device   uintptr
	queue    uintptr
	family   uint32
	mem      memoryProperties
	tsBits   uint32

	dsLayout   uint64
	pipeLayout uint64
	cmdPool    uint64
	descPool   uint64

	mu     sync.Mutex // coda (vkQueueSubmit non è rientrante sulla stessa coda)
	closed bool
}

var (
	libOnce sync.Once
	libH    uintptr
	libErr  error
)

func loadLib() (uintptr, error) {
	libOnce.Do(func() {
		for _, name := range []string{"libvulkan.so.1", "libvulkan.so"} {
			h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err == nil {
				libH = h
				return
			}
			libErr = err
		}
		libErr = fmt.Errorf("vulkan: libvulkan non trovata: %w", libErr)
	})
	return libH, libErr
}

func deviceTypeName(t uint32) string {
	switch t {
	case 1:
		return "integrata"
	case 2:
		return "discreta"
	case 3:
		return "virtuale"
	case 4:
		return "CPU"
	}
	return "altra"
}

// Open crea un device sulla GPU migliore: discreta, poi integrata, poi
// virtuale; le implementazioni software (llvmpipe) solo se
// TOBII4C_VK_ALLOW_CPU=1. TOBII4C_VK_DEVICE=n sceglie l'n-esimo device.
func Open() (*Device, error) {
	lib, err := loadLib()
	if err != nil {
		return nil, err
	}
	d := &Device{lib: lib}
	if d.fn.getInstanceProcAddr, err = purego.Dlsym(lib, "vkGetInstanceProcAddr"); err != nil {
		return nil, fmt.Errorf("vulkan: %w", err)
	}
	gipa := func(inst uintptr, name string) uintptr {
		r, _, _ := purego.SyscallN(d.fn.getInstanceProcAddr, inst, uintptr(unsafe.Pointer(cstr(name))))
		return r
	}
	d.fn.createInstance = gipa(0, "vkCreateInstance")
	d.fn.enumerateInstanceLayerProperties = gipa(0, "vkEnumerateInstanceLayerProperties")
	if d.fn.createInstance == 0 {
		return nil, errors.New("vulkan: vkCreateInstance mancante")
	}

	app := &applicationInfo{sType: stApplicationInfo, pApplicationName: cstr("tobii4c"), applicationVersion: 1,
		pEngineName: cstr("tobii4c-tflite"), engineVersion: 1, apiVersion: 1<<22 | 2<<12}
	ici := &instanceCreateInfo{sType: stInstanceCreateInfo, pApplicationInfo: app}
	var layers []*byte
	if os.Getenv("TOBII4C_VK_VALIDATION") == "1" && d.hasLayer("VK_LAYER_KHRONOS_validation") {
		layers = append(layers, cstr("VK_LAYER_KHRONOS_validation"))
		ici.enabledLayerCount, ici.ppEnabledLayerNames = 1, &layers[0]
	}
	if err := check("vkCreateInstance", call(d.fn.createInstance, uintptr(unsafe.Pointer(ici)), 0, uintptr(unsafe.Pointer(&d.instance)))); err != nil {
		return nil, err
	}
	runtime.KeepAlive(layers)
	in := d.instance
	f := &d.fn
	for _, e := range []struct {
		dst  *uintptr
		name string
	}{
		{&f.destroyInstance, "vkDestroyInstance"},
		{&f.enumeratePhysicalDevices, "vkEnumeratePhysicalDevices"},
		{&f.getPhysicalDeviceProperties2, "vkGetPhysicalDeviceProperties2"},
		{&f.getPhysicalDeviceQueueFamilyProps, "vkGetPhysicalDeviceQueueFamilyProperties"},
		{&f.getPhysicalDeviceMemoryProperties, "vkGetPhysicalDeviceMemoryProperties"},
		{&f.createDevice, "vkCreateDevice"},
		{&f.getDeviceProcAddr, "vkGetDeviceProcAddr"},
	} {
		if *e.dst = gipa(in, e.name); *e.dst == 0 {
			d.destroyInstance()
			return nil, fmt.Errorf("vulkan: %s mancante (serve Vulkan 1.1)", e.name)
		}
	}
	if err := d.pickDevice(); err != nil {
		d.destroyInstance()
		return nil, err
	}
	if err := d.createDevice(); err != nil {
		d.destroyInstance()
		return nil, err
	}
	if err := d.createLayouts(); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *Device) hasLayer(name string) bool {
	if d.fn.enumerateInstanceLayerProperties == 0 {
		return false
	}
	var n uint32
	call(d.fn.enumerateInstanceLayerProperties, uintptr(unsafe.Pointer(&n)), 0)
	if n == 0 {
		return false
	}
	buf := make([]byte, int(n)*520) // VkLayerProperties: 256 + 4 + 4 + 256
	call(d.fn.enumerateInstanceLayerProperties, uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0])))
	for i := 0; i < int(n); i++ {
		if goString(buf[i*520:i*520+256]) == name {
			return true
		}
	}
	return false
}

func (d *Device) destroyInstance() {
	if d.instance != 0 && d.fn.destroyInstance != 0 {
		call(d.fn.destroyInstance, d.instance, 0)
		d.instance = 0
	}
}

func (d *Device) pickDevice() error {
	var n uint32
	if err := check("vkEnumeratePhysicalDevices", call(d.fn.enumeratePhysicalDevices, d.instance, uintptr(unsafe.Pointer(&n)), 0)); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("vulkan: nessuna GPU")
	}
	devs := make([]uintptr, n)
	if err := check("vkEnumeratePhysicalDevices", call(d.fn.enumeratePhysicalDevices, d.instance, uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&devs[0])))); err != nil {
		return err
	}
	devs = devs[:n]
	allowCPU := os.Getenv("TOBII4C_VK_ALLOW_CPU") == "1"
	want := -1
	if s := os.Getenv("TOBII4C_VK_DEVICE"); s != "" {
		fmt.Sscanf(s, "%d", &want)
	}
	rank := map[uint32]int{2: 4, 1: 3, 3: 2, 0: 1, 4: 0}
	best, bestRank := -1, -1
	for i, pd := range devs {
		var dp driverProperties
		p2 := physicalDeviceProperties2{sType: stPhysicalDeviceProps2, pNext: unsafe.Pointer(&dp)}
		dp.sType = stPhysicalDeviceDriverProp
		call(d.fn.getPhysicalDeviceProperties2, pd, uintptr(unsafe.Pointer(&p2)))
		typ := le32(p2.props[16:])
		r := rank[typ]
		if typ == 4 && !allowCPU {
			r = -1
		}
		if want >= 0 {
			r = -1
			if i == want {
				r = 10
			}
		}
		if r > bestRank && d.computeFamily(pd) >= 0 {
			best, bestRank = i, r
		}
	}
	if best < 0 || bestRank < 0 {
		return errors.New("vulkan: nessuna GPU con una coda di calcolo (llvmpipe esclusa: TOBII4C_VK_ALLOW_CPU=1)")
	}
	d.physical = devs[best]
	var dp driverProperties
	dp.sType = stPhysicalDeviceDriverProp
	p2 := physicalDeviceProperties2{sType: stPhysicalDeviceProps2, pNext: unsafe.Pointer(&dp)}
	call(d.fn.getPhysicalDeviceProperties2, d.physical, uintptr(unsafe.Pointer(&p2)))
	pr := p2.props[:]
	api := le32(pr[0:])
	lim := pr[296:]
	in := &d.Info
	in.Name = goString(pr[20 : 20+256])
	in.VendorID, in.DeviceID = le32(pr[8:]), le32(pr[12:])
	in.Type = deviceTypeName(le32(pr[16:]))
	in.API = fmt.Sprintf("%d.%d.%d", api>>22&0x7f, api>>12&0x3ff, api&0xfff)
	in.Driver, in.DriverInfo = goString(dp.driverName[:]), goString(dp.driverInfo[:])
	in.SharedMem = int(le32(lim[216:]))
	in.MaxInvocations = int(le32(lim[232:]))
	tsPeriod := f32(lim[424:])
	d.family = uint32(d.computeFamily(d.physical))
	in.QueueFamily = int(d.family)
	if d.tsBits > 0 {
		in.TimestampNs = float64(tsPeriod)
	}
	call(d.fn.getPhysicalDeviceMemoryProperties, d.physical, uintptr(unsafe.Pointer(&d.mem)))
	for i := 0; i < int(d.mem.memoryHeapCount); i++ {
		h := d.mem.memoryHeaps[i]
		if h.flags&1 != 0 && h.size > in.DeviceMemory {
			in.DeviceMemory = h.size
		}
	}
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func f32(b []byte) float32 { return *(*float32)(unsafe.Pointer(&b[0])) }

// computeFamily è la prima famiglia di code con il calcolo (preferendo
// quella che ha anche i timestamp); -1 se non c'è.
func (d *Device) computeFamily(pd uintptr) int {
	var n uint32
	call(d.fn.getPhysicalDeviceQueueFamilyProps, pd, uintptr(unsafe.Pointer(&n)), 0)
	if n == 0 {
		return -1
	}
	fam := make([]queueFamilyProperties, n)
	call(d.fn.getPhysicalDeviceQueueFamilyProps, pd, uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&fam[0])))
	best := -1
	for i, f := range fam[:n] {
		if f.queueFlags&queueCompute == 0 || f.queueCount == 0 {
			continue
		}
		if best < 0 || (fam[best].timestampValidBits == 0 && f.timestampValidBits > 0) {
			best = i
		}
	}
	if best >= 0 && pd == d.physical {
		d.tsBits = fam[best].timestampValidBits
	}
	return best
}

func (d *Device) createDevice() error {
	prio := float32(1)
	qci := &deviceQueueCreateInfo{sType: stDeviceQueueCreateInfo, queueFamilyIndex: d.family, queueCount: 1, pQueuePriorities: &prio}
	dci := &deviceCreateInfo{sType: stDeviceCreateInfo, queueCreateInfoCount: 1, pQueueCreateInfos: qci}
	if err := check("vkCreateDevice", call(d.fn.createDevice, d.physical, uintptr(unsafe.Pointer(dci)), 0, uintptr(unsafe.Pointer(&d.device)))); err != nil {
		return err
	}
	gdpa := func(name string) uintptr {
		r, _, _ := purego.SyscallN(d.fn.getDeviceProcAddr, d.device, uintptr(unsafe.Pointer(cstr(name))))
		return r
	}
	f := &d.fn
	for _, e := range []struct {
		dst  *uintptr
		name string
	}{
		{&f.destroyDevice, "vkDestroyDevice"}, {&f.getDeviceQueue, "vkGetDeviceQueue"},
		{&f.createBuffer, "vkCreateBuffer"}, {&f.destroyBuffer, "vkDestroyBuffer"},
		{&f.getBufferMemoryRequirements, "vkGetBufferMemoryRequirements"},
		{&f.allocateMemory, "vkAllocateMemory"}, {&f.freeMemory, "vkFreeMemory"},
		{&f.bindBufferMemory, "vkBindBufferMemory"}, {&f.mapMemory, "vkMapMemory"}, {&f.unmapMemory, "vkUnmapMemory"},
		{&f.createShaderModule, "vkCreateShaderModule"}, {&f.destroyShaderModule, "vkDestroyShaderModule"},
		{&f.createDescriptorSetLayout, "vkCreateDescriptorSetLayout"},
		{&f.destroyDescriptorSetLayout, "vkDestroyDescriptorSetLayout"},
		{&f.createPipelineLayout, "vkCreatePipelineLayout"}, {&f.destroyPipelineLayout, "vkDestroyPipelineLayout"},
		{&f.createComputePipelines, "vkCreateComputePipelines"}, {&f.destroyPipeline, "vkDestroyPipeline"},
		{&f.createDescriptorPool, "vkCreateDescriptorPool"}, {&f.destroyDescriptorPool, "vkDestroyDescriptorPool"},
		{&f.allocateDescriptorSets, "vkAllocateDescriptorSets"}, {&f.updateDescriptorSets, "vkUpdateDescriptorSets"},
		{&f.createCommandPool, "vkCreateCommandPool"}, {&f.destroyCommandPool, "vkDestroyCommandPool"},
		{&f.allocateCommandBuffers, "vkAllocateCommandBuffers"}, {&f.freeCommandBuffers, "vkFreeCommandBuffers"},
		{&f.beginCommandBuffer, "vkBeginCommandBuffer"}, {&f.endCommandBuffer, "vkEndCommandBuffer"},
		{&f.cmdBindPipeline, "vkCmdBindPipeline"}, {&f.cmdBindDescriptorSets, "vkCmdBindDescriptorSets"},
		{&f.cmdPushConstants, "vkCmdPushConstants"}, {&f.cmdDispatch, "vkCmdDispatch"},
		{&f.cmdPipelineBarrier, "vkCmdPipelineBarrier"}, {&f.cmdCopyBuffer, "vkCmdCopyBuffer"},
		{&f.cmdFillBuffer, "vkCmdFillBuffer"},
		{&f.cmdWriteTimestamp, "vkCmdWriteTimestamp"}, {&f.cmdResetQueryPool, "vkCmdResetQueryPool"},
		{&f.queueSubmit, "vkQueueSubmit"}, {&f.queueWaitIdle, "vkQueueWaitIdle"},
		{&f.createFence, "vkCreateFence"}, {&f.destroyFence, "vkDestroyFence"},
		{&f.waitForFences, "vkWaitForFences"}, {&f.resetFences, "vkResetFences"},
		{&f.createQueryPool, "vkCreateQueryPool"}, {&f.destroyQueryPool, "vkDestroyQueryPool"},
		{&f.getQueryPoolResults, "vkGetQueryPoolResults"}, {&f.deviceWaitIdle, "vkDeviceWaitIdle"},
	} {
		if *e.dst = gdpa(e.name); *e.dst == 0 {
			return fmt.Errorf("vulkan: %s mancante", e.name)
		}
	}
	call(f.getDeviceQueue, d.device, uintptr(d.family), 0, uintptr(unsafe.Pointer(&d.queue)))
	cpci := &commandPoolCreateInfo{sType: stCommandPoolCreate, flags: 0x2 /* RESET_COMMAND_BUFFER */, queueFamilyIndex: d.family}
	return check("vkCreateCommandPool", call(f.createCommandPool, d.device, uintptr(unsafe.Pointer(cpci)), 0, uintptr(unsafe.Pointer(&d.cmdPool))))
}

// maxSets è il numero massimo di descriptor set (uno per modello caricato).
const maxSets = 64

func (d *Device) createLayouts() error {
	b := [2]descriptorSetLayoutBinding{
		{binding: 0, descriptorType: descStorageBuffer, descriptorCount: 1, stageFlags: stageCompute},
		{binding: 1, descriptorType: descStorageBuffer, descriptorCount: 1, stageFlags: stageCompute},
	}
	dsl := &descriptorSetLayoutCreateInfo{sType: stDescriptorSetLayout, bindingCount: 2, pBindings: &b[0]}
	if err := check("vkCreateDescriptorSetLayout", call(d.fn.createDescriptorSetLayout, d.device, uintptr(unsafe.Pointer(dsl)), 0, uintptr(unsafe.Pointer(&d.dsLayout)))); err != nil {
		return err
	}
	pcr := &pushConstantRange{stageFlags: stageCompute, size: PushSize}
	plci := &pipelineLayoutCreateInfo{sType: stPipelineLayoutCreate, setLayoutCount: 1, pSetLayouts: &d.dsLayout,
		pushConstantRangeCount: 1, pPushConstantRanges: pcr}
	if err := check("vkCreatePipelineLayout", call(d.fn.createPipelineLayout, d.device, uintptr(unsafe.Pointer(plci)), 0, uintptr(unsafe.Pointer(&d.pipeLayout)))); err != nil {
		return err
	}
	ps := &descriptorPoolSize{typ: descStorageBuffer, count: 2 * maxSets}
	dpci := &descriptorPoolCreateInfo{sType: stDescriptorPoolCreate, maxSets: maxSets, poolSizeCount: 1, pPoolSizes: ps}
	return check("vkCreateDescriptorPool", call(d.fn.createDescriptorPool, d.device, uintptr(unsafe.Pointer(dpci)), 0, uintptr(unsafe.Pointer(&d.descPool))))
}

// Close distrugge il device: tutti gli oggetti creati da lui devono essere
// già stati distrutti o non vanno più usati.
func (d *Device) Close() {
	if d == nil || d.closed {
		return
	}
	d.closed = true
	if d.device != 0 {
		call(d.fn.deviceWaitIdle, d.device)
		if d.descPool != 0 {
			call(d.fn.destroyDescriptorPool, d.device, uintptr(d.descPool), 0)
		}
		if d.pipeLayout != 0 {
			call(d.fn.destroyPipelineLayout, d.device, uintptr(d.pipeLayout), 0)
		}
		if d.dsLayout != 0 {
			call(d.fn.destroyDescriptorSetLayout, d.device, uintptr(d.dsLayout), 0)
		}
		if d.cmdPool != 0 {
			call(d.fn.destroyCommandPool, d.device, uintptr(d.cmdPool), 0)
		}
		call(d.fn.destroyDevice, d.device, 0)
		d.device = 0
	}
	d.destroyInstance()
}

// --- buffer -------------------------------------------------------------------

// Buffer è un buffer con la sua memoria. Host: memoria visibile dalla CPU
// (mappata in Bytes), altrimenti memoria locale della GPU.
type Buffer struct {
	d    *Device
	buf  uint64
	mem  uint64
	Size int
	Host bool
	data []byte
}

func (d *Device) memType(bits uint32, want, avoid uint32) int {
	for i := 0; i < int(d.mem.memoryTypeCount); i++ {
		f := d.mem.memoryTypes[i].propertyFlags
		if bits&(1<<i) != 0 && f&want == want && f&avoid == 0 {
			return i
		}
	}
	return -1
}

// NewBuffer crea un buffer di size byte. Quelli host stanno nella RAM di
// sistema (con cache se possibile), gli altri nella memoria della GPU.
func (d *Device) NewBuffer(size int, host bool) (*Buffer, error) {
	if size <= 0 {
		size = 4
	}
	size = (size + 255) &^ 255
	b := &Buffer{d: d, Size: size, Host: host}
	bci := &bufferCreateInfo{sType: stBufferCreateInfo, size: uint64(size),
		usage: bufUsageStorage | bufUsageTransferSrc | bufUsageTransferDst}
	if err := check("vkCreateBuffer", call(d.fn.createBuffer, d.device, uintptr(unsafe.Pointer(bci)), 0, uintptr(unsafe.Pointer(&b.buf)))); err != nil {
		return nil, err
	}
	var req memoryRequirements
	call(d.fn.getBufferMemoryRequirements, d.device, uintptr(b.buf), uintptr(unsafe.Pointer(&req)))
	var ti int
	if host {
		ti = d.memType(req.memoryTypeBits, memHostVisible|memHostCoherent|memHostCached, memDeviceLocal)
		if ti < 0 {
			ti = d.memType(req.memoryTypeBits, memHostVisible|memHostCoherent, memDeviceLocal)
		}
		if ti < 0 {
			ti = d.memType(req.memoryTypeBits, memHostVisible|memHostCoherent, 0)
		}
	} else {
		ti = d.memType(req.memoryTypeBits, memDeviceLocal, memHostVisible)
		if ti < 0 {
			ti = d.memType(req.memoryTypeBits, memDeviceLocal, 0)
		}
		if ti < 0 {
			ti = d.memType(req.memoryTypeBits, 0, 0)
		}
	}
	if ti < 0 {
		b.Destroy()
		return nil, errors.New("vulkan: nessun tipo di memoria adatto")
	}
	mai := &memoryAllocateInfo{sType: stMemoryAllocateInfo, allocationSize: req.size, memoryTypeIndex: uint32(ti)}
	if err := check("vkAllocateMemory", call(d.fn.allocateMemory, d.device, uintptr(unsafe.Pointer(mai)), 0, uintptr(unsafe.Pointer(&b.mem)))); err != nil {
		b.Destroy()
		return nil, err
	}
	if err := check("vkBindBufferMemory", call(d.fn.bindBufferMemory, d.device, uintptr(b.buf), uintptr(b.mem), 0)); err != nil {
		b.Destroy()
		return nil, err
	}
	if host {
		var p unsafe.Pointer // memoria mappata dal driver, fuori dallo heap di Go
		if err := check("vkMapMemory", call(d.fn.mapMemory, d.device, uintptr(b.mem), 0, uintptr(size), 0, uintptr(unsafe.Pointer(&p)))); err != nil {
			b.Destroy()
			return nil, err
		}
		b.data = unsafe.Slice((*byte)(p), size)
	}
	return b, nil
}

// Bytes è la memoria mappata (solo buffer host): vale finché il buffer esiste.
func (b *Buffer) Bytes() []byte { return b.data }

// Float32 è la memoria mappata come float32 (solo buffer host).
func (b *Buffer) Float32() []float32 {
	if b.data == nil {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&b.data[0])), len(b.data)/4)
}

// Destroy libera il buffer.
func (b *Buffer) Destroy() {
	if b == nil || b.d == nil {
		return
	}
	d := b.d
	if b.data != nil {
		call(d.fn.unmapMemory, d.device, uintptr(b.mem))
		b.data = nil
	}
	if b.buf != 0 {
		call(d.fn.destroyBuffer, d.device, uintptr(b.buf), 0)
	}
	if b.mem != 0 {
		call(d.fn.freeMemory, d.device, uintptr(b.mem), 0)
	}
	b.d = nil
}

// --- pipeline e descriptor set ------------------------------------------------

// Pipeline è una pipeline di compute con il layout comune.
type Pipeline struct {
	d *Device
	p uint64
}

// NewPipeline crea una pipeline da SPIR-V (entry point "main").
func (d *Device) NewPipeline(spirv []byte) (*Pipeline, error) {
	if len(spirv) < 20 || len(spirv)%4 != 0 || le32(spirv) != 0x07230203 {
		return nil, errors.New("vulkan: SPIR-V non valido")
	}
	code := make([]uint32, len(spirv)/4)
	for i := range code {
		code[i] = le32(spirv[4*i:])
	}
	smci := &shaderModuleCreateInfo{sType: stShaderModuleCreateInfo, codeSize: uintptr(len(spirv)), pCode: &code[0]}
	var mod uint64
	if err := check("vkCreateShaderModule", call(d.fn.createShaderModule, d.device, uintptr(unsafe.Pointer(smci)), 0, uintptr(unsafe.Pointer(&mod)))); err != nil {
		return nil, err
	}
	defer call(d.fn.destroyShaderModule, d.device, uintptr(mod), 0)
	cpci := &computePipelineCreateInfo{sType: stComputePipelineCreate, layout: d.pipeLayout, basePipelineIndex: -1,
		stage: pipelineShaderStageCreateInfo{sType: stPipelineShaderStage, stage: stageCompute, module: mod, pName: cstr("main")}}
	p := &Pipeline{d: d}
	if err := check("vkCreateComputePipelines", call(d.fn.createComputePipelines, d.device, 0, 1, uintptr(unsafe.Pointer(cpci)), 0, uintptr(unsafe.Pointer(&p.p)))); err != nil {
		return nil, err
	}
	return p, nil
}

// Destroy libera la pipeline.
func (p *Pipeline) Destroy() {
	if p != nil && p.d != nil && p.p != 0 {
		call(p.d.fn.destroyPipeline, p.d.device, uintptr(p.p), 0)
		p.p = 0
	}
}

// Set lega due buffer ai binding 0 e 1 del layout comune.
type Set struct {
	s uint64
}

// NewSet crea un descriptor set con b0 e b1 (b1 può essere b0).
func (d *Device) NewSet(b0, b1 *Buffer) (*Set, error) {
	dsai := &descriptorSetAllocateInfo{sType: stDescriptorSetAllocate, descriptorPool: d.descPool, descriptorSetCount: 1, pSetLayouts: &d.dsLayout}
	s := &Set{}
	if err := check("vkAllocateDescriptorSets", call(d.fn.allocateDescriptorSets, d.device, uintptr(unsafe.Pointer(dsai)), uintptr(unsafe.Pointer(&s.s)))); err != nil {
		return nil, err
	}
	bi := [2]descriptorBufferInfo{{buffer: b0.buf, rng: ^uint64(0)}, {buffer: b1.buf, rng: ^uint64(0)}}
	w := [2]writeDescriptorSet{
		{sType: stWriteDescriptorSet, dstSet: s.s, dstBinding: 0, descriptorCount: 1, descriptorType: descStorageBuffer, pBufferInfo: &bi[0]},
		{sType: stWriteDescriptorSet, dstSet: s.s, dstBinding: 1, descriptorCount: 1, descriptorType: descStorageBuffer, pBufferInfo: &bi[1]},
	}
	call(d.fn.updateDescriptorSets, d.device, 2, uintptr(unsafe.Pointer(&w[0])), 0, 0)
	return s, nil
}

// --- programmi ----------------------------------------------------------------

// Program è un command buffer registrato una volta e rieseguibile.
type Program struct {
	d     *Device
	cmd   uintptr
	fence uint64
	qpool uint64
	nts   int
	si    submitInfo
}

// Recorder registra i comandi di un Program.
type Recorder struct {
	d    *Device
	cmd  uintptr
	p    *Program
	push [PushSize]byte
	err  error
}

// Record registra un programma; nts è il numero di timestamp (0: nessuno).
func (d *Device) Record(nts int, fn func(r *Recorder)) (*Program, error) {
	p := &Program{d: d}
	cbai := &commandBufferAllocateInfo{sType: stCommandBufferAllocate, commandPool: d.cmdPool, commandBufferCount: 1}
	if err := check("vkAllocateCommandBuffers", call(d.fn.allocateCommandBuffers, d.device, uintptr(unsafe.Pointer(cbai)), uintptr(unsafe.Pointer(&p.cmd)))); err != nil {
		return nil, err
	}
	fci := &fenceCreateInfo{sType: stFenceCreateInfo}
	if err := check("vkCreateFence", call(d.fn.createFence, d.device, uintptr(unsafe.Pointer(fci)), 0, uintptr(unsafe.Pointer(&p.fence)))); err != nil {
		p.Destroy()
		return nil, err
	}
	if nts > 0 && d.Info.TimestampNs > 0 {
		qpci := &queryPoolCreateInfo{sType: stQueryPoolCreateInfo, queryType: queryTimestamp, queryCount: uint32(nts)}
		if err := check("vkCreateQueryPool", call(d.fn.createQueryPool, d.device, uintptr(unsafe.Pointer(qpci)), 0, uintptr(unsafe.Pointer(&p.qpool)))); err != nil {
			p.Destroy()
			return nil, err
		}
		p.nts = nts
	}
	cbbi := &commandBufferBeginInfo{sType: stCommandBufferBegin}
	if err := check("vkBeginCommandBuffer", call(d.fn.beginCommandBuffer, p.cmd, uintptr(unsafe.Pointer(cbbi)))); err != nil {
		p.Destroy()
		return nil, err
	}
	r := &Recorder{d: d, cmd: p.cmd, p: p}
	if p.nts > 0 {
		call(d.fn.cmdResetQueryPool, p.cmd, uintptr(p.qpool), 0, uintptr(p.nts))
	}
	fn(r)
	// i risultati scritti dalla GPU devono essere visibili alla CPU
	r.barrier(pipeStageCompute|pipeStageTransfer, pipeStageHost, accShaderWrite|accTransferWrite, accHostRead)
	if err := check("vkEndCommandBuffer", call(d.fn.endCommandBuffer, p.cmd)); err != nil && r.err == nil {
		r.err = err
	}
	if r.err != nil {
		p.Destroy()
		return nil, r.err
	}
	p.si = submitInfo{sType: stSubmitInfo, commandBufferCount: 1, pCommandBuffers: &p.cmd}
	return p, nil
}

// Bind lega il descriptor set ai dispatch successivi.
func (r *Recorder) Bind(s *Set) {
	call(r.d.fn.cmdBindDescriptorSets, r.cmd, 1 /* COMPUTE */, uintptr(r.d.pipeLayout), 0, 1, uintptr(unsafe.Pointer(&s.s)), 0, 0)
}

// Dispatch esegue p con le push constant push (al più PushSize byte, in
// parole a 32 bit) su gx×gy×gz workgroup, poi una barriera: il dispatch
// successivo vede i risultati.
func (r *Recorder) Dispatch(p *Pipeline, push []uint32, gx, gy, gz int) {
	if gx <= 0 || gy <= 0 || gz <= 0 {
		return
	}
	if len(push)*4 > PushSize {
		r.err = fmt.Errorf("vulkan: %d byte di push constant", len(push)*4)
		return
	}
	call(r.d.fn.cmdBindPipeline, r.cmd, 1, uintptr(p.p))
	if len(push) > 0 {
		for i, v := range push {
			r.push[4*i], r.push[4*i+1], r.push[4*i+2], r.push[4*i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
		}
		call(r.d.fn.cmdPushConstants, r.cmd, uintptr(r.d.pipeLayout), stageCompute, 0, uintptr(len(push)*4), uintptr(unsafe.Pointer(&r.push[0])))
	}
	call(r.d.fn.cmdDispatch, r.cmd, uintptr(gx), uintptr(gy), uintptr(gz))
	r.barrier(pipeStageCompute|pipeStageTransfer, pipeStageCompute|pipeStageTransfer,
		accShaderWrite|accTransferWrite, accShaderRead|accShaderWrite|accTransferRead|accTransferWrite)
}

// Copy copia n byte tra buffer, seguita da una barriera.
func (r *Recorder) Copy(src *Buffer, srcOff int, dst *Buffer, dstOff int, n int) {
	if n <= 0 {
		return
	}
	bc := &bufferCopy{srcOffset: uint64(srcOff), dstOffset: uint64(dstOff), size: uint64(n)}
	call(r.d.fn.cmdCopyBuffer, r.cmd, uintptr(src.buf), uintptr(dst.buf), 1, uintptr(unsafe.Pointer(bc)))
	r.barrier(pipeStageCompute|pipeStageTransfer, pipeStageCompute|pipeStageTransfer,
		accShaderWrite|accTransferWrite, accShaderRead|accShaderWrite|accTransferRead|accTransferWrite)
}

// Fill riempie n byte (multiplo di 4) con la parola v.
func (r *Recorder) Fill(dst *Buffer, off, n int, v uint32) {
	if n <= 0 {
		return
	}
	call(r.d.fn.cmdFillBuffer, r.cmd, uintptr(dst.buf), uintptr(off), uintptr(n), uintptr(v))
	r.barrier(pipeStageTransfer, pipeStageCompute|pipeStageTransfer, accTransferWrite,
		accShaderRead|accShaderWrite|accTransferRead|accTransferWrite)
}

// Timestamp scrive il timestamp i quando i comandi precedenti sono finiti.
func (r *Recorder) Timestamp(i int) {
	if i < 0 || i >= r.p.nts {
		return
	}
	call(r.d.fn.cmdWriteTimestamp, r.cmd, pipeStageBottom, uintptr(r.p.qpool), uintptr(i))
}

func (r *Recorder) barrier(src, dst, srcAcc, dstAcc uint32) {
	mb := &memoryBarrier{sType: stMemoryBarrier, srcAccessMask: srcAcc, dstAccessMask: dstAcc}
	call(r.d.fn.cmdPipelineBarrier, r.cmd, uintptr(src), uintptr(dst), 0, 1, uintptr(unsafe.Pointer(mb)), 0, 0, 0, 0)
}

// Run esegue il programma e aspetta che finisca. Le scritture della CPU nei
// buffer host fatte prima sono visibili alla GPU; i risultati sono
// leggibili al ritorno. Sicuro per l'uso concorrente (la coda è serializzata).
func (p *Program) Run() error {
	d := p.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := check("vkQueueSubmit", call(d.fn.queueSubmit, d.queue, 1, uintptr(unsafe.Pointer(&p.si)), uintptr(p.fence))); err != nil {
		return err
	}
	const timeout = 5_000_000_000 // 5 s
	r := call(d.fn.waitForFences, d.device, 1, uintptr(unsafe.Pointer(&p.fence)), 1, timeout)
	if r == vkTimeout {
		return &Error{"vkWaitForFences", r}
	}
	if err := check("vkWaitForFences", r); err != nil {
		return err
	}
	return check("vkResetFences", call(d.fn.resetFences, d.device, 1, uintptr(unsafe.Pointer(&p.fence))))
}

// Timestamps legge i timestamp dell'ultima esecuzione in nanosecondi
// (relativi al primo); dst deve avere almeno nts elementi.
func (p *Program) Timestamps(dst []float64) error {
	if p.nts == 0 {
		return errors.New("vulkan: timestamp non disponibili")
	}
	raw := make([]uint64, p.nts)
	if err := check("vkGetQueryPoolResults", call(p.d.fn.getQueryPoolResults, p.d.device, uintptr(p.qpool), 0, uintptr(p.nts),
		uintptr(8*p.nts), uintptr(unsafe.Pointer(&raw[0])), 8, queryResult64|queryWait)); err != nil {
		return err
	}
	mask := ^uint64(0)
	if b := p.d.tsBits; b > 0 && b < 64 {
		mask = 1<<b - 1
	}
	for i := 0; i < p.nts && i < len(dst); i++ {
		dst[i] = float64((raw[i]-raw[0])&mask) * p.d.Info.TimestampNs
	}
	return nil
}

// HasTimestamps dice se il programma registra i timestamp.
func (p *Program) HasTimestamps() bool { return p.nts > 0 }

// Destroy libera il programma.
func (p *Program) Destroy() {
	if p == nil || p.d == nil {
		return
	}
	d := p.d
	if p.qpool != 0 {
		call(d.fn.destroyQueryPool, d.device, uintptr(p.qpool), 0)
	}
	if p.fence != 0 {
		call(d.fn.destroyFence, d.device, uintptr(p.fence), 0)
	}
	if p.cmd != 0 {
		call(d.fn.freeCommandBuffers, d.device, uintptr(d.cmdPool), 1, uintptr(unsafe.Pointer(&p.cmd)))
	}
	p.d = nil
}

// Upload copia data nel buffer dst all'offset off passando da un buffer di
// appoggio (dst può stare nella memoria della GPU).
func (d *Device) Upload(dst *Buffer, off int, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if dst.Host {
		copy(dst.data[off:], data)
		return nil
	}
	const chunk = 16 << 20
	st, err := d.NewBuffer(min(len(data), chunk), true)
	if err != nil {
		return err
	}
	defer st.Destroy()
	for len(data) > 0 {
		n := min(len(data), chunk)
		copy(st.data, data[:n])
		p, err := d.Record(0, func(r *Recorder) { r.Copy(st, 0, dst, off, (n+3)&^3) })
		if err != nil {
			return err
		}
		err = p.Run()
		p.Destroy()
		if err != nil {
			return err
		}
		data, off = data[n:], off+n
	}
	return nil
}
