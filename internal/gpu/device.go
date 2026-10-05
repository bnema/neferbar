// Package gpu renders the cell grid with raw Vulkan into DMA-BUF images that
// the Wayland layer can hand to the compositor.
package gpu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"github.com/bnema/purego-vulkan/vulkan"
)

// requiredExtensions are mandatory on the device that matches the compositor's
// linux-dmabuf main device.
var requiredExtensions = []string{
	"VK_KHR_external_memory_fd",
	"VK_EXT_external_memory_dma_buf",
	"VK_EXT_image_drm_format_modifier",
}

// Device is the Vulkan device chosen by the compositor's main_device.
type Device struct {
	instance vulkan.Instance
	idisp    *vulkan.InstanceDispatch
	phys     vulkan.PhysicalDevice
	logical  vulkan.Device
	disp     *vulkan.DeviceDispatch
	queue    vulkan.Queue
	family   uint32
	// RenderMinor is the minor of the matching /dev/dri/renderD node.
	RenderMinor uint32

	memProps vulkan.PhysicalDeviceMemoryProperties
}

func devMajor(d uint64) uint32 { return uint32((d >> 8) & 0xfff) }
func devMinor(d uint64) uint32 { return uint32((d & 0xff) | ((d >> 12) & 0xffffff00)) }

// Open selects the Vulkan device whose primary or render node matches
// mainDevice (a dev_t). There is no cross-GPU fallback.
func Open(mainDevice uint64) (_ *Device, err error) {
	if err = vulkan.Init(); err != nil {
		return nil, err
	}
	name := []byte("neferbar\x00")
	ai := vulkan.ApplicationInfo{SType: vulkan.StructureTypeApplicationInfo, ApplicationName: &name[0], ApiVersion: 1<<22 | 3<<12}
	ci := vulkan.InstanceCreateInfo{SType: vulkan.StructureTypeInstanceCreateInfo, ApplicationInfo: &ai}
	d := &Device{}
	if err = vulkan.Check(vulkan.Global().CreateInstance(&ci, nil, &d.instance)); err != nil {
		return nil, fmt.Errorf("gpu: create Vulkan 1.3 instance: %w", err)
	}
	if d.idisp, err = vulkan.LoadInstanceDispatch(d.instance); err != nil {
		vulkan.VkDestroyInstance(d.instance, nil)
		return nil, err
	}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	if err = d.pickPhysical(mainDevice); err != nil {
		return nil, err
	}
	if err = d.createLogical(); err != nil {
		return nil, err
	}
	d.idisp.GetPhysicalDeviceMemoryProperties(d.phys, &d.memProps)
	return d, nil
}

func (d *Device) pickPhysical(main uint64) error {
	var count uint32
	if err := vulkan.Check(d.idisp.EnumeratePhysicalDevices(d.instance, &count, nil)); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("gpu: no Vulkan physical device")
	}
	all := make([]vulkan.PhysicalDevice, count)
	if err := vulkan.Check(d.idisp.EnumeratePhysicalDevices(d.instance, &count, &all[0])); err != nil {
		return err
	}
	mj, mn := devMajor(main), devMinor(main)
	for _, cand := range all[:count] {
		exts, err := d.deviceExtensions(cand)
		if err != nil {
			return err
		}
		if !exts["VK_EXT_physical_device_drm"] {
			continue
		}
		props := vulkan.PhysicalDeviceProperties2{SType: vulkan.StructureTypePhysicalDeviceProperties2}
		drm := vulkan.PhysicalDeviceDrmPropertiesEXT{SType: vulkan.StructureTypePhysicalDeviceDRMPropertiesEXT}
		props.Next = unsafe.Pointer(&drm)
		d.idisp.GetPhysicalDeviceProperties2(cand, &props)
		primary := drm.HasPrimary != 0 && uint32(drm.PrimaryMajor) == mj && uint32(drm.PrimaryMinor) == mn
		render := drm.HasRender != 0 && uint32(drm.RenderMajor) == mj && uint32(drm.RenderMinor) == mn
		if !primary && !render {
			continue
		}
		if drm.HasRender == 0 {
			return fmt.Errorf("gpu: device matching main_device %d:%d has no render node", mj, mn)
		}
		for _, need := range requiredExtensions {
			if !exts[need] {
				return fmt.Errorf("gpu: matching device lacks %s", need)
			}
		}
		d.phys, d.RenderMinor = cand, uint32(drm.RenderMinor)
		return nil
	}
	return fmt.Errorf("gpu: no Vulkan device matches linux-dmabuf main_device %d:%d", mj, mn)
}

func (d *Device) deviceExtensions(p vulkan.PhysicalDevice) (map[string]bool, error) {
	var count uint32
	if err := vulkan.Check(d.idisp.EnumerateDeviceExtensionProperties(p, nil, &count, nil)); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if count == 0 {
		return out, nil
	}
	props := make([]vulkan.ExtensionProperties, count)
	if err := vulkan.Check(d.idisp.EnumerateDeviceExtensionProperties(p, nil, &count, &props[0])); err != nil {
		return nil, err
	}
	for _, e := range props[:count] {
		out[string(bytes.TrimRight(e.ExtensionName[:], "\x00"))] = true
	}
	return out, nil
}

func (d *Device) createLogical() error {
	var families uint32
	d.idisp.GetPhysicalDeviceQueueFamilyProperties(d.phys, &families, nil)
	if families == 0 {
		return fmt.Errorf("gpu: no queue family")
	}
	qs := make([]vulkan.QueueFamilyProperties, families)
	d.idisp.GetPhysicalDeviceQueueFamilyProperties(d.phys, &families, &qs[0])
	found := false
	for i, q := range qs[:families] {
		if q.QueueCount != 0 && q.QueueFlags&vulkan.QueueGraphicsBit != 0 {
			d.family, found = uint32(i), true
			break
		}
	}
	if !found {
		return fmt.Errorf("gpu: no graphics queue")
	}
	sync2 := vulkan.PhysicalDeviceSynchronization2Features{SType: vulkan.StructureTypePhysicalDeviceSynchronization2Features, Synchronization2: 1}
	dyn := vulkan.PhysicalDeviceDynamicRenderingFeatures{SType: vulkan.StructureTypePhysicalDeviceDynamicRenderingFeatures, Next: unsafe.Pointer(&sync2), DynamicRendering: 1}
	prio := float32(1)
	qci := vulkan.DeviceQueueCreateInfo{SType: vulkan.StructureTypeDeviceQueueCreateInfo, QueueFamilyIndex: d.family, QueueCount: 1, QueuePriorities: &prio}
	names := make([][]byte, len(requiredExtensions))
	ptrs := make([]*byte, len(requiredExtensions))
	for i, n := range requiredExtensions {
		names[i] = append([]byte(n), 0)
		ptrs[i] = &names[i][0]
	}
	dci := vulkan.DeviceCreateInfo{SType: vulkan.StructureTypeDeviceCreateInfo, Next: unsafe.Pointer(&dyn), QueueCreateInfoCount: 1, QueueCreateInfos: &qci, EnabledExtensionCount: uint32(len(ptrs)), PpEnabledExtensionNames: &ptrs[0]}
	if err := vulkan.Check(d.idisp.CreateDevice(d.phys, &dci, nil, &d.logical)); err != nil {
		return fmt.Errorf("gpu: create device: %w", err)
	}
	var err error
	if d.disp, err = vulkan.LoadDeviceDispatch(d.idisp, d.logical); err != nil {
		return err
	}
	d.disp.GetDeviceQueue(d.logical, d.family, 0, &d.queue)
	return nil
}

// memoryType finds a memory type index allowed by bits with all want flags.
func (d *Device) memoryType(bits uint32, want vulkan.MemoryPropertyFlags) (uint32, error) {
	for i := uint32(0); i < d.memProps.MemoryTypeCount; i++ {
		if bits&(1<<i) != 0 && d.memProps.MemoryTypes[i].PropertyFlags&want == want {
			return i, nil
		}
	}
	return math.MaxUint32, fmt.Errorf("gpu: no memory type for flags %#x", want)
}

// WaitIdle blocks until the queue has finished all submitted work.
func (d *Device) WaitIdle() error { return vulkan.Check(d.disp.DeviceWaitIdle(d.logical)) }

// Close destroys the device and the instance.
func (d *Device) Close() {
	if d == nil {
		return
	}
	if d.logical != 0 {
		_ = d.WaitIdle()
		d.disp.DestroyDevice(d.logical, nil)
		d.logical = 0
	}
	if d.instance != 0 {
		d.idisp.DestroyInstance(d.instance, nil)
		d.instance = 0
	}
}

// Modifier is a DRM format modifier both the GPU and the compositor accept.
type Modifier uint64

// ExportableModifiers lists single-plane modifiers of B8G8R8A8 the GPU can
// render to and export as DMA-BUF.
func (d *Device) ExportableModifiers() (map[uint64]bool, error) {
	props := vulkan.FormatProperties2{SType: vulkan.StructureTypeFormatProperties2}
	list := vulkan.DrmFormatModifierPropertiesList2EXT{SType: vulkan.StructureTypeDRMFormatModifierPropertiesList2EXT}
	props.Next = unsafe.Pointer(&list)
	d.idisp.GetPhysicalDeviceFormatProperties2(d.phys, vulkan.FormatB8g8r8a8Unorm, &props)
	out := map[uint64]bool{}
	if list.DrmFormatModifierCount == 0 {
		return out, nil
	}
	mods := make([]vulkan.DrmFormatModifierProperties2EXT, list.DrmFormatModifierCount)
	list.DrmFormatModifierProperties = &mods[0]
	d.idisp.GetPhysicalDeviceFormatProperties2(d.phys, vulkan.FormatB8g8r8a8Unorm, &props)
	for _, m := range mods[:list.DrmFormatModifierCount] {
		if m.DrmFormatModifierPlaneCount != 1 || m.DrmFormatModifierTilingFeatures&vulkan.FormatFeature2ColorAttachmentBit == 0 {
			continue
		}
		mi := vulkan.PhysicalDeviceImageDrmFormatModifierInfoEXT{SType: vulkan.StructureTypePhysicalDeviceImageDRMFormatModifierInfoEXT, DrmFormatModifier: m.DrmFormatModifier, SharingMode: vulkan.SharingModeExclusive}
		ei := vulkan.PhysicalDeviceExternalImageFormatInfo{SType: vulkan.StructureTypePhysicalDeviceExternalImageFormatInfo, Next: unsafe.Pointer(&mi), HandleType: vulkan.ExternalMemoryHandleTypeDMABUFBitEXT}
		info := vulkan.PhysicalDeviceImageFormatInfo2{SType: vulkan.StructureTypePhysicalDeviceImageFormatInfo2, Next: unsafe.Pointer(&ei), Format: vulkan.FormatB8g8r8a8Unorm, Type: vulkan.ImageType2d, Tiling: vulkan.ImageTilingDRMFormatModifierEXT, Usage: vulkan.ImageUsageColorAttachmentBit}
		ext := vulkan.ExternalImageFormatProperties{SType: vulkan.StructureTypeExternalImageFormatProperties}
		ip := vulkan.ImageFormatProperties2{SType: vulkan.StructureTypeImageFormatProperties2, Next: unsafe.Pointer(&ext)}
		r := d.idisp.GetPhysicalDeviceImageFormatProperties2(d.phys, &info, &ip)
		if r == vulkan.ErrorFormatNotSupported {
			continue
		}
		if err := vulkan.Check(r); err != nil {
			return nil, fmt.Errorf("gpu: query modifier %#x: %w", m.DrmFormatModifier, err)
		}
		if ext.ExternalMemoryProperties.ExternalMemoryFeatures&vulkan.ExternalMemoryFeatureExportableBit != 0 {
			out[m.DrmFormatModifier] = true
		}
	}
	return out, nil
}

// DRMFormatXRGB8888 is the fourcc of XRGB8888, which maps to B8G8R8A8.
const DRMFormatXRGB8888 uint32 = 0x34325258

// FormatPair is one DRM format and modifier the compositor accepts.
type FormatPair struct {
	FourCC   uint32
	Modifier uint64
}

// ChooseModifier returns the first compositor-preferred XRGB8888 modifier the
// GPU can export.
func ChooseModifier(formats []FormatPair, supported map[uint64]bool) (Modifier, error) {
	for _, f := range formats {
		if f.FourCC == DRMFormatXRGB8888 && supported[f.Modifier] {
			return Modifier(f.Modifier), nil
		}
	}
	return 0, fmt.Errorf("gpu: no XRGB8888 modifier shared by the GPU and the compositor")
}

func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
