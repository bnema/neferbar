package gpu

import (
	"fmt"
	"math"
	"unsafe"

	"github.com/bnema/purego-vulkan/vulkan"
	"golang.org/x/sys/unix"
)

// Image is a render target backed by exportable DMA-BUF memory. FD stays owned
// by the Image; the Wayland layer duplicates it on import.
type Image struct {
	dev    *Device
	image  vulkan.Image
	memory vulkan.DeviceMemory
	view   vulkan.ImageView

	Width, Height  int32
	FD             int
	Modifier       uint64
	Offset, Stride uint32

	// used is false until the first render: the first transition starts from
	// an undefined layout.
	used bool
}

// NewImage allocates a Width x Height B8G8R8A8 image with the given modifier.
func (d *Device) NewImage(width, height int32, mod Modifier) (img *Image, err error) {
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("gpu: invalid image size %dx%d", width, height)
	}
	img = &Image{dev: d, Width: width, Height: height, FD: -1}
	defer func() {
		if err != nil {
			img.Close()
			img = nil
		}
	}()
	modifier := uint64(mod)
	list := vulkan.ImageDrmFormatModifierListCreateInfoEXT{SType: vulkan.StructureTypeImageDRMFormatModifierListCreateInfoEXT, DrmFormatModifierCount: 1, DrmFormatModifiers: &modifier}
	ext := vulkan.ExternalMemoryImageCreateInfo{SType: vulkan.StructureTypeExternalMemoryImageCreateInfo, Next: unsafe.Pointer(&list), HandleTypes: vulkan.ExternalMemoryHandleTypeDMABUFBitEXT}
	ci := vulkan.ImageCreateInfo{
		SType: vulkan.StructureTypeImageCreateInfo, Next: unsafe.Pointer(&ext),
		ImageType: vulkan.ImageType2d, Format: vulkan.FormatB8g8r8a8Unorm,
		Extent:    vulkan.Extent3D{Width: uint32(width), Height: uint32(height), Depth: 1},
		MipLevels: 1, ArrayLayers: 1, Samples: vulkan.SampleCount1Bit,
		Tiling: vulkan.ImageTilingDRMFormatModifierEXT, Usage: vulkan.ImageUsageColorAttachmentBit,
		SharingMode: vulkan.SharingModeExclusive, InitialLayout: vulkan.ImageLayoutUndefined,
	}
	if err = vulkan.Check(d.disp.CreateImage(d.logical, &ci, nil, &img.image)); err != nil {
		return nil, fmt.Errorf("gpu: create image: %w", err)
	}
	props := vulkan.ImageDrmFormatModifierPropertiesEXT{SType: vulkan.StructureTypeImageDRMFormatModifierPropertiesEXT}
	if err = vulkan.Check(d.disp.GetImageDrmFormatModifierPropertiesEXT(d.logical, img.image, &props)); err != nil {
		return nil, err
	}
	if props.DrmFormatModifier != modifier {
		return nil, fmt.Errorf("gpu: asked for modifier %#x, driver chose %#x", modifier, props.DrmFormatModifier)
	}
	img.Modifier = modifier
	sub := vulkan.ImageSubresource{AspectMask: vulkan.ImageAspectMemoryPlane0BitEXT}
	var layout vulkan.SubresourceLayout
	d.disp.GetImageSubresourceLayout(d.logical, img.image, &sub, &layout)
	if layout.RowPitch == 0 || layout.RowPitch > math.MaxUint32 || layout.Offset > math.MaxUint32 {
		return nil, fmt.Errorf("gpu: unusable plane layout offset %d stride %d", layout.Offset, layout.RowPitch)
	}
	img.Offset, img.Stride = uint32(layout.Offset), uint32(layout.RowPitch)

	reqInfo := vulkan.ImageMemoryRequirementsInfo2{SType: vulkan.StructureTypeImageMemoryRequirementsInfo2, Image: img.image}
	reqs := vulkan.MemoryRequirements2{SType: vulkan.StructureTypeMemoryRequirements2}
	d.disp.GetImageMemoryRequirements2(d.logical, &reqInfo, &reqs)
	idx, err := d.memoryType(reqs.MemoryRequirements.MemoryTypeBits, vulkan.MemoryPropertyDeviceLocalBit)
	if err != nil {
		return nil, err
	}
	ded := vulkan.MemoryDedicatedAllocateInfo{SType: vulkan.StructureTypeMemoryDedicatedAllocateInfo, Image: img.image}
	exp := vulkan.ExportMemoryAllocateInfo{SType: vulkan.StructureTypeExportMemoryAllocateInfo, Next: unsafe.Pointer(&ded), HandleTypes: vulkan.ExternalMemoryHandleTypeDMABUFBitEXT}
	ai := vulkan.MemoryAllocateInfo{SType: vulkan.StructureTypeMemoryAllocateInfo, Next: unsafe.Pointer(&exp), AllocationSize: reqs.MemoryRequirements.Size, MemoryTypeIndex: idx}
	if err = vulkan.Check(d.disp.AllocateMemory(d.logical, &ai, nil, &img.memory)); err != nil {
		return nil, fmt.Errorf("gpu: allocate image memory: %w", err)
	}
	bind := vulkan.BindImageMemoryInfo{SType: vulkan.StructureTypeBindImageMemoryInfo, Image: img.image, Memory: img.memory}
	if err = vulkan.Check(d.disp.BindImageMemory2(d.logical, 1, &bind)); err != nil {
		return nil, fmt.Errorf("gpu: bind image memory: %w", err)
	}
	fdi := vulkan.MemoryGetFdInfoKHR{SType: vulkan.StructureTypeMemoryGetFDInfoKHR, Memory: img.memory, HandleType: vulkan.ExternalMemoryHandleTypeDMABUFBitEXT}
	fd := int32(-1)
	if err = vulkan.Check(d.disp.GetMemoryFdKHR(d.logical, &fdi, &fd)); err != nil {
		return nil, fmt.Errorf("gpu: export DMA-BUF: %w", err)
	}
	img.FD = int(fd)
	vi := vulkan.ImageViewCreateInfo{
		SType: vulkan.StructureTypeImageViewCreateInfo, Image: img.image, ViewType: vulkan.ImageViewType2d,
		Format:           vulkan.FormatB8g8r8a8Unorm,
		SubresourceRange: vulkan.ImageSubresourceRange{AspectMask: vulkan.ImageAspectColorBit, LevelCount: 1, LayerCount: 1},
	}
	if err = vulkan.Check(d.disp.CreateImageView(d.logical, &vi, nil, &img.view)); err != nil {
		return nil, err
	}
	return img, nil
}

// Close frees the image. The GPU must be done with it; the compositor's import
// holds its own kernel reference.
func (i *Image) Close() {
	if i == nil || i.dev == nil {
		return
	}
	d := i.dev
	if i.FD >= 0 {
		_ = unix.Close(i.FD)
		i.FD = -1
	}
	if i.view != 0 {
		d.disp.DestroyImageView(d.logical, i.view, nil)
		i.view = 0
	}
	if i.image != 0 {
		d.disp.DestroyImage(d.logical, i.image, nil)
		i.image = 0
	}
	if i.memory != 0 {
		d.disp.FreeMemory(d.logical, i.memory, nil)
		i.memory = 0
	}
}
