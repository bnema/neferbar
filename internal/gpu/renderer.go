package gpu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/bnema/purego-vulkan/vulkan"
	"golang.org/x/sys/unix"

	"git.bnema.dev/bnema/neferbar/internal/glyph"
	"git.bnema.dev/bnema/neferbar/internal/gpu/shaders"
	"git.bnema.dev/bnema/neferbar/internal/syncobj"
)

const (
	atlasSize = 2048
	// SlotCount is the number of DMA-BUF images: one on screen, one being
	// shown or queued, one being drawn.
	SlotCount = 3
	// maxUploads bounds the glyphs rasterized by one Draw.
	maxUploads = 256
)

// Cell is one grid cell as the renderer consumes it.
type Cell struct {
	Rune   rune
	FG, BG [3]uint8
	Style  glyph.Style
}

// instance is the per-cell vertex data (64 bytes, four vec4).
type instance struct {
	Rect [4]float32 // x, y, w, h in pixels
	UV   [4]float32 // atlas u0, v0, u1, v1
	FG   [4]float32
	BG   [4]float32
}

type glyphKey struct {
	r     rune
	style glyph.Style
}

type slotState uint8

const (
	slotFree slotState = iota
	slotShown
)

// slot is one DMA-BUF image with everything needed to draw and present it.
type slot struct {
	img   *Image
	pool  vulkan.CommandPool
	cmd   vulkan.CommandBuffer
	fence vulkan.Fence

	instBuf, stageBuf vulkan.Buffer
	instMem, stageMem vulkan.DeviceMemory
	inst              []instance // mapped
	stage             []byte     // mapped

	releaseHandle uint32
	releaseFD     int // syncobj fd, exported once
	eventFD       int // readable when releasePoint signals
	releasePoint  uint64
	presented     bool
	state         slotState

	// Scratch argument structs: the bindings heap-allocate pointer arguments,
	// so a call built from locals allocates on every frame.
	begin    vulkan.CommandBufferBeginInfo
	barrier  [1]vulkan.ImageMemoryBarrier2
	dep      vulkan.DependencyInfo
	color    vulkan.RenderingAttachmentInfo
	render   vulkan.RenderingInfo
	cmdInfo  vulkan.CommandBufferSubmitInfo
	submit   vulkan.SubmitInfo2
	offset   vulkan.DeviceSize
	push     [2]float32
	regions  []vulkan.BufferImageCopy
	sets     [1]vulkan.DescriptorSet
	atlasSub vulkan.ImageSubresourceRange
}

// Frame describes one drawn slot for the Wayland layer. Fields stay valid
// until the next Draw.
type Frame struct {
	Slot            int
	NewBuffer       bool // import Image as a wl_buffer
	Image           *Image
	AcquirePoint    uint64
	ReleasePoint    uint64
	ReleaseEventFD  int // readable when ReleasePoint signals; stable per slot
	ReleaseTimeline int // syncobj fd of the slot's release timeline; valid with NewBuffer
}

// Renderer draws a row of cells into a pool of DMA-BUF images.
type Renderer struct {
	dev  *Device
	node *syncobj.Node
	face *glyph.Face

	W, H          int32
	cols, maxCols int
	yOff          float32

	slots [SlotCount]slot

	acquireHandle uint32
	acquireFD     int
	acquirePoint  uint64

	// atlas
	atlasImg    vulkan.Image
	atlasMem    vulkan.DeviceMemory
	atlasView   vulkan.ImageView
	sampler     vulkan.Sampler
	setLayout   vulkan.DescriptorSetLayout
	descPool    vulkan.DescriptorPool
	descSet     vulkan.DescriptorSet
	pipeLayout  vulkan.PipelineLayout
	pipeline    vulkan.Pipeline
	atlasCols   int
	atlasRows   int
	glyphs      map[glyphKey]uint32
	nextGlyph   uint32
	atlasInited bool

	uploadKeys []glyphKey // glyphs added during the current Draw
	uploadIdx  []uint32
}

// NewRenderer builds the pipeline, the atlas and the image pool for a w x h
// target. node must be the render node of dev.
func NewRenderer(dev *Device, node *syncobj.Node, mod Modifier, face *glyph.Face, w, h int32) (r *Renderer, err error) {
	if face.CellH > int(h) {
		return nil, fmt.Errorf("gpu: cell height %d exceeds bar height %d", face.CellH, h)
	}
	r = &Renderer{dev: dev, node: node, face: face, W: w, H: h, acquireFD: -1,
		glyphs: make(map[glyphKey]uint32, 256), yOff: float32((int(h) - face.CellH) / 2)}
	r.cols = (int(w) + face.CellW - 1) / face.CellW
	r.maxCols = r.cols
	for i := range r.slots {
		r.slots[i].releaseFD, r.slots[i].eventFD = -1, -1
	}
	defer func() {
		if err != nil {
			r.Close()
			r = nil
		}
	}()
	if err = r.newAtlas(); err != nil {
		return nil, err
	}
	if err = r.newPipeline(); err != nil {
		return nil, err
	}
	if r.acquireHandle, err = node.Create(); err != nil {
		return nil, err
	}
	if r.acquireFD, err = node.Export(r.acquireHandle); err != nil {
		return nil, err
	}
	for i := range r.slots {
		if err = r.newSlot(i, mod); err != nil {
			return nil, err
		}
	}
	// Glyph 0 is the blank cell. Printable ASCII is assigned now and uploaded
	// with the first frame.
	r.glyphs[glyphKey{' ', 0}] = 0
	r.nextGlyph = 1
	for c := rune(0x21); c < 0x7f; c++ {
		r.glyphs[glyphKey{c, 0}] = r.nextGlyph
		r.nextGlyph++
	}
	r.uploadKeys = make([]glyphKey, 0, maxUploads)
	r.uploadIdx = make([]uint32, 0, maxUploads)
	return r, nil
}

func (r *Renderer) buffer(size vulkan.DeviceSize, usage vulkan.BufferUsageFlags) (vulkan.Buffer, vulkan.DeviceMemory, unsafe.Pointer, error) {
	d := r.dev
	var buf vulkan.Buffer
	ci := vulkan.BufferCreateInfo{SType: vulkan.StructureTypeBufferCreateInfo, Size: size, Usage: usage, SharingMode: vulkan.SharingModeExclusive}
	if err := vulkan.Check(d.disp.CreateBuffer(d.logical, &ci, nil, &buf)); err != nil {
		return 0, 0, nil, err
	}
	var req vulkan.MemoryRequirements
	d.disp.GetBufferMemoryRequirements(d.logical, buf, &req)
	idx, err := d.memoryType(req.MemoryTypeBits, vulkan.MemoryPropertyHostVisibleBit|vulkan.MemoryPropertyHostCoherentBit)
	if err != nil {
		d.disp.DestroyBuffer(d.logical, buf, nil)
		return 0, 0, nil, err
	}
	var mem vulkan.DeviceMemory
	ai := vulkan.MemoryAllocateInfo{SType: vulkan.StructureTypeMemoryAllocateInfo, AllocationSize: req.Size, MemoryTypeIndex: idx}
	if err = vulkan.Check(d.disp.AllocateMemory(d.logical, &ai, nil, &mem)); err != nil {
		d.disp.DestroyBuffer(d.logical, buf, nil)
		return 0, 0, nil, err
	}
	fail := func(err error) (vulkan.Buffer, vulkan.DeviceMemory, unsafe.Pointer, error) {
		d.disp.FreeMemory(d.logical, mem, nil)
		d.disp.DestroyBuffer(d.logical, buf, nil)
		return 0, 0, nil, err
	}
	if err = vulkan.Check(d.disp.BindBufferMemory(d.logical, buf, mem, 0)); err != nil {
		return fail(err)
	}
	var ptr unsafe.Pointer
	if err = vulkan.Check(d.disp.MapMemory(d.logical, mem, 0, size, 0, &ptr)); err != nil {
		return fail(err)
	}
	return buf, mem, ptr, nil
}

func (r *Renderer) newSlot(i int, mod Modifier) (err error) {
	d := r.dev
	s := &r.slots[i]
	if s.img, err = d.NewImage(r.W, r.H, mod); err != nil {
		return err
	}
	pi := vulkan.CommandPoolCreateInfo{SType: vulkan.StructureTypeCommandPoolCreateInfo, QueueFamilyIndex: d.family, Flags: vulkan.CommandPoolCreateResetCommandBufferBit}
	if err = vulkan.Check(d.disp.CreateCommandPool(d.logical, &pi, nil, &s.pool)); err != nil {
		return err
	}
	ai := vulkan.CommandBufferAllocateInfo{SType: vulkan.StructureTypeCommandBufferAllocateInfo, CommandPool: s.pool, Level: vulkan.CommandBufferLevelPrimary, CommandBufferCount: 1}
	if err = vulkan.Check(d.disp.AllocateCommandBuffers(d.logical, &ai, &s.cmd)); err != nil {
		return err
	}
	fi := vulkan.FenceCreateInfo{SType: vulkan.StructureTypeFenceCreateInfo}
	if err = vulkan.Check(d.disp.CreateFence(d.logical, &fi, nil, &s.fence)); err != nil {
		return err
	}
	instBytes := vulkan.DeviceSize(r.maxCols) * vulkan.DeviceSize(unsafe.Sizeof(instance{}))
	var p unsafe.Pointer
	if s.instBuf, s.instMem, p, err = r.buffer(instBytes, vulkan.BufferUsageVertexBufferBit); err != nil {
		return err
	}
	s.inst = unsafe.Slice((*instance)(p), r.maxCols)
	stageBytes := maxUploads * r.face.CellW * r.face.CellH
	if s.stageBuf, s.stageMem, p, err = r.buffer(vulkan.DeviceSize(stageBytes), vulkan.BufferUsageTransferSrcBit); err != nil {
		return err
	}
	s.stage = unsafe.Slice((*byte)(p), stageBytes)
	s.regions = make([]vulkan.BufferImageCopy, 0, maxUploads)
	s.atlasSub = vulkan.ImageSubresourceRange{AspectMask: vulkan.ImageAspectColorBit, LevelCount: 1, LayerCount: 1}
	if s.releaseHandle, err = r.node.Create(); err != nil {
		return err
	}
	if s.releaseFD, err = r.node.Export(s.releaseHandle); err != nil {
		return err
	}
	if s.eventFD, err = unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK); err != nil {
		return err
	}
	return nil
}

func (r *Renderer) newAtlas() error {
	d := r.dev
	r.atlasCols = atlasSize / r.face.CellW
	r.atlasRows = atlasSize / r.face.CellH
	if r.atlasCols < 1 || r.atlasRows < 1 {
		return fmt.Errorf("gpu: cell %dx%d does not fit the atlas", r.face.CellW, r.face.CellH)
	}
	ci := vulkan.ImageCreateInfo{
		SType: vulkan.StructureTypeImageCreateInfo, ImageType: vulkan.ImageType2d, Format: vulkan.FormatR8Unorm,
		Extent:    vulkan.Extent3D{Width: atlasSize, Height: atlasSize, Depth: 1},
		MipLevels: 1, ArrayLayers: 1, Samples: vulkan.SampleCount1Bit, Tiling: vulkan.ImageTilingOptimal,
		Usage:       vulkan.ImageUsageSampledBit | vulkan.ImageUsageTransferDstBit,
		SharingMode: vulkan.SharingModeExclusive, InitialLayout: vulkan.ImageLayoutUndefined,
	}
	if err := vulkan.Check(d.disp.CreateImage(d.logical, &ci, nil, &r.atlasImg)); err != nil {
		return fmt.Errorf("gpu: create atlas: %w", err)
	}
	var req vulkan.MemoryRequirements
	d.disp.GetImageMemoryRequirements(d.logical, r.atlasImg, &req)
	idx, err := d.memoryType(req.MemoryTypeBits, vulkan.MemoryPropertyDeviceLocalBit)
	if err != nil {
		return err
	}
	ai := vulkan.MemoryAllocateInfo{SType: vulkan.StructureTypeMemoryAllocateInfo, AllocationSize: req.Size, MemoryTypeIndex: idx}
	if err = vulkan.Check(d.disp.AllocateMemory(d.logical, &ai, nil, &r.atlasMem)); err != nil {
		return err
	}
	if err = vulkan.Check(d.disp.BindImageMemory(d.logical, r.atlasImg, r.atlasMem, 0)); err != nil {
		return err
	}
	vi := vulkan.ImageViewCreateInfo{
		SType: vulkan.StructureTypeImageViewCreateInfo, Image: r.atlasImg, ViewType: vulkan.ImageViewType2d, Format: vulkan.FormatR8Unorm,
		SubresourceRange: vulkan.ImageSubresourceRange{AspectMask: vulkan.ImageAspectColorBit, LevelCount: 1, LayerCount: 1},
	}
	if err = vulkan.Check(d.disp.CreateImageView(d.logical, &vi, nil, &r.atlasView)); err != nil {
		return err
	}
	si := vulkan.SamplerCreateInfo{
		SType: vulkan.StructureTypeSamplerCreateInfo, MagFilter: vulkan.FilterNearest, MinFilter: vulkan.FilterNearest,
		MipmapMode:   vulkan.SamplerMipmapModeNearest,
		AddressModeU: vulkan.SamplerAddressModeClampToEdge, AddressModeV: vulkan.SamplerAddressModeClampToEdge, AddressModeW: vulkan.SamplerAddressModeClampToEdge,
	}
	if err = vulkan.Check(d.disp.CreateSampler(d.logical, &si, nil, &r.sampler)); err != nil {
		return err
	}
	b := vulkan.DescriptorSetLayoutBinding{DescriptorType: vulkan.DescriptorTypeCombinedImageSampler, DescriptorCount: 1, StageFlags: vulkan.ShaderStageFragmentBit}
	li := vulkan.DescriptorSetLayoutCreateInfo{SType: vulkan.StructureTypeDescriptorSetLayoutCreateInfo, BindingCount: 1, Bindings: &b}
	if err = vulkan.Check(d.disp.CreateDescriptorSetLayout(d.logical, &li, nil, &r.setLayout)); err != nil {
		return err
	}
	ps := vulkan.DescriptorPoolSize{Type: vulkan.DescriptorTypeCombinedImageSampler, DescriptorCount: 1}
	pi := vulkan.DescriptorPoolCreateInfo{SType: vulkan.StructureTypeDescriptorPoolCreateInfo, MaxSets: 1, PoolSizeCount: 1, PoolSizes: &ps}
	if err = vulkan.Check(d.disp.CreateDescriptorPool(d.logical, &pi, nil, &r.descPool)); err != nil {
		return err
	}
	ds := vulkan.DescriptorSetAllocateInfo{SType: vulkan.StructureTypeDescriptorSetAllocateInfo, DescriptorPool: r.descPool, DescriptorSetCount: 1, SetLayouts: &r.setLayout}
	if err = vulkan.Check(d.disp.AllocateDescriptorSets(d.logical, &ds, &r.descSet)); err != nil {
		return err
	}
	info := vulkan.DescriptorImageInfo{Sampler: r.sampler, ImageView: r.atlasView, ImageLayout: vulkan.ImageLayoutShaderReadOnlyOptimal}
	w := vulkan.WriteDescriptorSet{SType: vulkan.StructureTypeWriteDescriptorSet, DstSet: r.descSet, DescriptorCount: 1, DescriptorType: vulkan.DescriptorTypeCombinedImageSampler, ImageInfo: &info}
	d.disp.UpdateDescriptorSets(d.logical, 1, &w, 0, nil)
	return nil
}

func (r *Renderer) shader(code []byte) (vulkan.ShaderModule, error) {
	if len(code) < 20 || len(code)%4 != 0 || binary.LittleEndian.Uint32(code) != 0x07230203 {
		return 0, errors.New("gpu: invalid embedded SPIR-V")
	}
	words := make([]uint32, len(code)/4)
	for i := range words {
		words[i] = le32(code[i*4:])
	}
	ci := vulkan.ShaderModuleCreateInfo{SType: vulkan.StructureTypeShaderModuleCreateInfo, CodeSize: uintptr(len(code)), Code: &words[0]}
	var m vulkan.ShaderModule
	if err := vulkan.Check(r.dev.disp.CreateShaderModule(r.dev.logical, &ci, nil, &m)); err != nil {
		return 0, err
	}
	return m, nil
}

func (r *Renderer) newPipeline() error {
	d := r.dev
	pc := vulkan.PushConstantRange{StageFlags: vulkan.ShaderStageVertexBit, Size: 8}
	li := vulkan.PipelineLayoutCreateInfo{SType: vulkan.StructureTypePipelineLayoutCreateInfo, SetLayoutCount: 1, SetLayouts: &r.setLayout, PushConstantRangeCount: 1, PushConstantRanges: &pc}
	if err := vulkan.Check(d.disp.CreatePipelineLayout(d.logical, &li, nil, &r.pipeLayout)); err != nil {
		return err
	}
	vs, err := r.shader(shaders.CellVertex)
	if err != nil {
		return err
	}
	defer d.disp.DestroyShaderModule(d.logical, vs, nil)
	fs, err := r.shader(shaders.CellFragment)
	if err != nil {
		return err
	}
	defer d.disp.DestroyShaderModule(d.logical, fs, nil)
	entry := []byte("main\x00")
	stages := [2]vulkan.PipelineShaderStageCreateInfo{
		{SType: vulkan.StructureTypePipelineShaderStageCreateInfo, Stage: vulkan.ShaderStageVertexBit, Module: vs, Name: &entry[0]},
		{SType: vulkan.StructureTypePipelineShaderStageCreateInfo, Stage: vulkan.ShaderStageFragmentBit, Module: fs, Name: &entry[0]},
	}
	var attrs [4]vulkan.VertexInputAttributeDescription
	for i := range attrs {
		attrs[i] = vulkan.VertexInputAttributeDescription{Location: uint32(i), Format: vulkan.FormatR32g32b32a32Sfloat, Offset: uint32(i) * 16}
	}
	bind := vulkan.VertexInputBindingDescription{Stride: uint32(unsafe.Sizeof(instance{})), InputRate: vulkan.VertexInputRateInstance}
	vin := vulkan.PipelineVertexInputStateCreateInfo{SType: vulkan.StructureTypePipelineVertexInputStateCreateInfo, VertexBindingDescriptionCount: 1, VertexBindingDescriptions: &bind, VertexAttributeDescriptionCount: 4, VertexAttributeDescriptions: &attrs[0]}
	asm := vulkan.PipelineInputAssemblyStateCreateInfo{SType: vulkan.StructureTypePipelineInputAssemblyStateCreateInfo, Topology: vulkan.PrimitiveTopologyTriangleList}
	vp := vulkan.Viewport{Width: float32(r.W), Height: float32(r.H), MaxDepth: 1}
	sc := vulkan.Rect2D{Extent: vulkan.Extent2D{Width: uint32(r.W), Height: uint32(r.H)}}
	vps := vulkan.PipelineViewportStateCreateInfo{SType: vulkan.StructureTypePipelineViewportStateCreateInfo, ViewportCount: 1, Viewports: &vp, ScissorCount: 1, Scissors: &sc}
	ras := vulkan.PipelineRasterizationStateCreateInfo{SType: vulkan.StructureTypePipelineRasterizationStateCreateInfo, PolygonMode: vulkan.PolygonModeFill, CullMode: vulkan.CullModeNone, FrontFace: vulkan.FrontFaceCounterClockwise, LineWidth: 1}
	ms := vulkan.PipelineMultisampleStateCreateInfo{SType: vulkan.StructureTypePipelineMultisampleStateCreateInfo, RasterizationSamples: vulkan.SampleCount1Bit}
	att := vulkan.PipelineColorBlendAttachmentState{ColorWriteMask: vulkan.ColorComponentRBit | vulkan.ColorComponentGBit | vulkan.ColorComponentBBit | vulkan.ColorComponentABit}
	blend := vulkan.PipelineColorBlendStateCreateInfo{SType: vulkan.StructureTypePipelineColorBlendStateCreateInfo, AttachmentCount: 1, Attachments: &att}
	format := vulkan.Format(vulkan.FormatB8g8r8a8Unorm)
	rend := vulkan.PipelineRenderingCreateInfo{SType: vulkan.StructureTypePipelineRenderingCreateInfo, ColorAttachmentCount: 1, ColorAttachmentFormats: &format}
	gi := vulkan.GraphicsPipelineCreateInfo{
		SType: vulkan.StructureTypeGraphicsPipelineCreateInfo, Next: unsafe.Pointer(&rend), StageCount: 2, Stages: &stages[0],
		VertexInputState: &vin, InputAssemblyState: &asm, ViewportState: &vps, RasterizationState: &ras,
		MultisampleState: &ms, ColorBlendState: &blend, Layout: r.pipeLayout,
	}
	if err = vulkan.Check(d.disp.CreateGraphicsPipelines(d.logical, 0, 1, &gi, nil, &r.pipeline)); err != nil {
		return fmt.Errorf("gpu: create pipeline: %w", err)
	}
	return nil
}

// queueGlyph schedules k for upload to atlas index idx in the current Draw.
func (r *Renderer) queueGlyph(k glyphKey, idx uint32) {
	r.uploadKeys = append(r.uploadKeys, k)
	r.uploadIdx = append(r.uploadIdx, idx)
}

// glyph returns the atlas index of k, assigning one (and queueing its upload)
// on first use. An exhausted atlas maps to the blank glyph.
func (r *Renderer) glyph(k glyphKey) uint32 {
	if idx, ok := r.glyphs[k]; ok {
		return idx
	}
	if int(r.nextGlyph) >= r.atlasCols*r.atlasRows || len(r.uploadKeys) >= maxUploads {
		return 0
	}
	idx := r.nextGlyph
	r.nextGlyph++
	r.glyphs[k] = idx
	r.queueGlyph(k, idx)
	return idx
}

// pickSlot returns a free slot index, or -1.
func (r *Renderer) pickSlot() int {
	for i := range r.slots {
		if r.slots[i].state == slotFree {
			return i
		}
	}
	return -1
}

// Draw renders cells (len(cells) <= cols) into a free slot, waits for the GPU
// and signals the acquire point. It reports false when every image is still
// held by the compositor. clear is the background for pixels outside cells.
func (r *Renderer) Draw(cells []Cell, clear [3]uint8, out *Frame) (bool, error) {
	n := r.pickSlot()
	if n < 0 {
		return false, nil
	}
	s := &r.slots[n]
	if len(cells) > r.maxCols {
		cells = cells[:r.maxCols]
	}
	r.uploadKeys, r.uploadIdx = r.uploadKeys[:0], r.uploadIdx[:0]
	if !r.atlasInited {
		r.queueGlyph(glyphKey{' ', 0}, 0)
		for c := rune(0x21); c < 0x7f; c++ {
			r.queueGlyph(glyphKey{c, 0}, r.glyphs[glyphKey{c, 0}])
		}
	}
	cw, ch := float32(r.face.CellW), float32(r.face.CellH)
	inv := float32(1) / atlasSize
	for i := range cells {
		c := &cells[i]
		gi := r.glyph(glyphKey{c.Rune, c.Style})
		gx, gy := float32(int(gi)%r.atlasCols)*cw, float32(int(gi)/r.atlasCols)*ch
		in := &s.inst[i]
		in.Rect = [4]float32{float32(i) * cw, r.yOff, cw, ch}
		in.UV = [4]float32{gx * inv, gy * inv, (gx + cw) * inv, (gy + ch) * inv}
		in.FG = [4]float32{float32(c.FG[0]) / 255, float32(c.FG[1]) / 255, float32(c.FG[2]) / 255, 1}
		in.BG = [4]float32{float32(c.BG[0]) / 255, float32(c.BG[1]) / 255, float32(c.BG[2]) / 255, 1}
	}
	cellBytes := r.face.CellW * r.face.CellH
	s.regions = s.regions[:0]
	for i, k := range r.uploadKeys {
		off := i * cellBytes
		r.face.Rasterize(k.r, k.style, s.stage[off:off+cellBytes])
		gi := int(r.uploadIdx[i])
		s.regions = append(s.regions, vulkan.BufferImageCopy{
			BufferOffset:     vulkan.DeviceSize(off),
			BufferRowLength:  uint32(r.face.CellW),
			ImageSubresource: vulkan.ImageSubresourceLayers{AspectMask: vulkan.ImageAspectColorBit, LayerCount: 1},
			ImageOffset:      vulkan.Offset3D{X: int32((gi % r.atlasCols) * r.face.CellW), Y: int32((gi / r.atlasCols) * r.face.CellH)},
			ImageExtent:      vulkan.Extent3D{Width: uint32(r.face.CellW), Height: uint32(r.face.CellH), Depth: 1},
		})
	}
	if err := r.record(s, len(cells), clear); err != nil {
		return false, err
	}
	d := r.dev
	s.cmdInfo = vulkan.CommandBufferSubmitInfo{SType: vulkan.StructureTypeCommandBufferSubmitInfo, CommandBuffer: s.cmd}
	s.submit = vulkan.SubmitInfo2{SType: vulkan.StructureTypeSubmitInfo2, CommandBufferInfoCount: 1, CommandBufferInfos: &s.cmdInfo}
	if err := vulkan.Check(d.disp.QueueSubmit2(d.queue, 1, &s.submit, s.fence)); err != nil {
		return false, fmt.Errorf("gpu: submit: %w", err)
	}
	// The draw is tiny: wait for it here, then signal the acquire point from
	// the CPU. No sync-file import is needed.
	if err := vulkan.Check(d.disp.WaitForFences(d.logical, 1, &s.fence, 1, math.MaxUint64)); err != nil {
		return false, fmt.Errorf("gpu: wait fence: %w", err)
	}
	if err := vulkan.Check(d.disp.ResetFences(d.logical, 1, &s.fence)); err != nil {
		return false, err
	}
	r.atlasInited = true
	r.acquirePoint++
	if err := r.node.Signal(r.acquireHandle, r.acquirePoint); err != nil {
		return false, err
	}
	s.releasePoint++
	if err := r.node.EventFD(s.releaseHandle, s.releasePoint, s.eventFD); err != nil {
		return false, err
	}
	s.state = slotShown
	*out = Frame{
		Slot: n, NewBuffer: !s.presented, Image: s.img,
		AcquirePoint: r.acquirePoint, ReleasePoint: s.releasePoint,
		ReleaseEventFD: s.eventFD, ReleaseTimeline: s.releaseFD,
	}
	s.presented = true
	return true, nil
}

func (r *Renderer) record(s *slot, count int, clearRGB [3]uint8) error {
	d := r.dev
	disp := d.disp
	if err := vulkan.Check(disp.ResetCommandBuffer(s.cmd, 0)); err != nil {
		return err
	}
	s.begin = vulkan.CommandBufferBeginInfo{SType: vulkan.StructureTypeCommandBufferBeginInfo, Flags: vulkan.CommandBufferUsageOneTimeSubmitBit}
	if err := vulkan.Check(disp.BeginCommandBuffer(s.cmd, &s.begin)); err != nil {
		return err
	}
	s.dep = vulkan.DependencyInfo{SType: vulkan.StructureTypeDependencyInfo, ImageMemoryBarrierCount: 1, ImageMemoryBarriers: &s.barrier[0]}
	barrier := &s.barrier[0]
	if len(s.regions) > 0 || !r.atlasInited {
		old := vulkan.ImageLayout(vulkan.ImageLayoutShaderReadOnlyOptimal)
		srcStage, srcAccess := vulkan.PipelineStageFlags2(vulkan.PipelineStage2FragmentShaderBit), vulkan.AccessFlags2(vulkan.Access2ShaderSampledReadBit)
		if !r.atlasInited {
			old, srcStage, srcAccess = vulkan.ImageLayoutUndefined, vulkan.PipelineStage2None, 0
		}
		*barrier = vulkan.ImageMemoryBarrier2{
			SType: vulkan.StructureTypeImageMemoryBarrier2, SrcStageMask: srcStage, SrcAccessMask: srcAccess,
			DstStageMask: vulkan.PipelineStage2AllTransferBit, DstAccessMask: vulkan.Access2TransferWriteBit,
			OldLayout: old, NewLayout: vulkan.ImageLayoutTransferDstOptimal,
			SrcQueueFamilyIndex: ^uint32(0), DstQueueFamilyIndex: ^uint32(0), Image: r.atlasImg, SubresourceRange: s.atlasSub,
		}
		disp.CmdPipelineBarrier2(s.cmd, &s.dep)
		if !r.atlasInited {
			var zero vulkan.ClearColorValue
			disp.CmdClearColorImage(s.cmd, r.atlasImg, vulkan.ImageLayoutTransferDstOptimal, &zero, 1, &s.atlasSub)
			barrier.SrcStageMask, barrier.SrcAccessMask = vulkan.PipelineStage2AllTransferBit, vulkan.Access2TransferWriteBit
			barrier.OldLayout = vulkan.ImageLayoutTransferDstOptimal
			disp.CmdPipelineBarrier2(s.cmd, &s.dep)
		}
		if len(s.regions) > 0 {
			disp.CmdCopyBufferToImage(s.cmd, s.stageBuf, r.atlasImg, vulkan.ImageLayoutTransferDstOptimal, uint32(len(s.regions)), &s.regions[0])
		}
		barrier.SrcStageMask, barrier.SrcAccessMask = vulkan.PipelineStage2AllTransferBit, vulkan.Access2TransferWriteBit
		barrier.DstStageMask, barrier.DstAccessMask = vulkan.PipelineStage2FragmentShaderBit, vulkan.Access2ShaderSampledReadBit
		barrier.OldLayout, barrier.NewLayout = vulkan.ImageLayoutTransferDstOptimal, vulkan.ImageLayoutShaderReadOnlyOptimal
		disp.CmdPipelineBarrier2(s.cmd, &s.dep)
	}
	old := vulkan.ImageLayout(vulkan.ImageLayoutGeneral)
	if !s.img.used {
		old = vulkan.ImageLayoutUndefined
	}
	*barrier = vulkan.ImageMemoryBarrier2{
		SType: vulkan.StructureTypeImageMemoryBarrier2, SrcStageMask: vulkan.PipelineStage2None,
		DstStageMask: vulkan.PipelineStage2ColorAttachmentOutputBit, DstAccessMask: vulkan.Access2ColorAttachmentWriteBit,
		OldLayout: old, NewLayout: vulkan.ImageLayoutColorAttachmentOptimal,
		SrcQueueFamilyIndex: ^uint32(0), DstQueueFamilyIndex: ^uint32(0), Image: s.img.image, SubresourceRange: s.atlasSub,
	}
	disp.CmdPipelineBarrier2(s.cmd, &s.dep)
	var cv vulkan.ClearValue
	cv[0], cv[1], cv[2], cv[3] = math.Float32bits(float32(clearRGB[0])/255), math.Float32bits(float32(clearRGB[1])/255), math.Float32bits(float32(clearRGB[2])/255), math.Float32bits(1)
	s.color = vulkan.RenderingAttachmentInfo{
		SType: vulkan.StructureTypeRenderingAttachmentInfo, ImageView: s.img.view, ImageLayout: vulkan.ImageLayoutColorAttachmentOptimal,
		LoadOp: vulkan.AttachmentLoadOpClear, StoreOp: vulkan.AttachmentStoreOpStore, ClearValue: cv,
	}
	s.render = vulkan.RenderingInfo{
		SType:      vulkan.StructureTypeRenderingInfo,
		RenderArea: vulkan.Rect2D{Extent: vulkan.Extent2D{Width: uint32(r.W), Height: uint32(r.H)}},
		LayerCount: 1, ColorAttachmentCount: 1, ColorAttachments: &s.color,
	}
	disp.CmdBeginRendering(s.cmd, &s.render)
	disp.CmdBindPipeline(s.cmd, vulkan.PipelineBindPointGraphics, r.pipeline)
	s.sets[0] = r.descSet
	disp.CmdBindDescriptorSets(s.cmd, vulkan.PipelineBindPointGraphics, r.pipeLayout, 0, 1, &s.sets[0], 0, nil)
	s.push = [2]float32{float32(r.W), float32(r.H)}
	disp.CmdPushConstants(s.cmd, r.pipeLayout, vulkan.ShaderStageVertexBit, 0, 8, unsafe.Pointer(&s.push))
	s.offset = 0
	disp.CmdBindVertexBuffers(s.cmd, 0, 1, &s.instBuf, &s.offset)
	if count > 0 {
		disp.CmdDraw(s.cmd, 6, uint32(count), 0, 0)
	}
	disp.CmdEndRendering(s.cmd)
	// Leave the image in GENERAL for the compositor.
	barrier.SrcStageMask, barrier.SrcAccessMask = vulkan.PipelineStage2ColorAttachmentOutputBit, vulkan.Access2ColorAttachmentWriteBit
	barrier.DstStageMask, barrier.DstAccessMask = vulkan.PipelineStage2AllCommandsBit, vulkan.Access2MemoryReadBit
	barrier.OldLayout, barrier.NewLayout = vulkan.ImageLayoutColorAttachmentOptimal, vulkan.ImageLayoutGeneral
	disp.CmdPipelineBarrier2(s.cmd, &s.dep)
	s.img.used = true
	return vulkan.Check(disp.EndCommandBuffer(s.cmd))
}

// Released handles a readable release eventfd of slot: when the compositor's
// release point has signaled, the image may be drawn again.
func (r *Renderer) Released(slotIdx int) error {
	if slotIdx < 0 || slotIdx >= SlotCount {
		return nil
	}
	s := &r.slots[slotIdx]
	var counter [8]byte
	if _, err := unix.Read(s.eventFD, counter[:]); err != nil && !errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("gpu: read release eventfd: %w", err)
	}
	if s.state != slotShown {
		return nil
	}
	point, err := r.node.Query(s.releaseHandle)
	if err != nil {
		return err
	}
	if point >= s.releasePoint {
		s.state = slotFree
	}
	return nil
}

// AcquireTimelineFD is the syncobj fd of the shared acquire timeline.
func (r *Renderer) AcquireTimelineFD() int { return r.acquireFD }

// Cols is the number of cells that fit the width, rounding up.
func (r *Renderer) Cols() int { return r.cols }

// Close waits for the GPU and frees everything. The compositor's imports keep
// their own kernel references.
func (r *Renderer) Close() {
	if r == nil {
		return
	}
	d := r.dev
	_ = d.WaitIdle()
	for i := range r.slots {
		s := &r.slots[i]
		if s.eventFD >= 0 {
			_ = unix.Close(s.eventFD)
		}
		if s.releaseFD >= 0 {
			_ = unix.Close(s.releaseFD)
		}
		if s.releaseHandle != 0 {
			_ = r.node.Destroy(s.releaseHandle)
		}
		for _, b := range [...]struct {
			buf vulkan.Buffer
			mem vulkan.DeviceMemory
		}{{s.instBuf, s.instMem}, {s.stageBuf, s.stageMem}} {
			if b.mem != 0 {
				d.disp.UnmapMemory(d.logical, b.mem)
				d.disp.FreeMemory(d.logical, b.mem, nil)
			}
			if b.buf != 0 {
				d.disp.DestroyBuffer(d.logical, b.buf, nil)
			}
		}
		if s.fence != 0 {
			d.disp.DestroyFence(d.logical, s.fence, nil)
		}
		if s.pool != 0 {
			d.disp.DestroyCommandPool(d.logical, s.pool, nil)
		}
		s.img.Close()
	}
	if r.acquireFD >= 0 {
		_ = unix.Close(r.acquireFD)
	}
	if r.acquireHandle != 0 {
		_ = r.node.Destroy(r.acquireHandle)
	}
	if r.pipeline != 0 {
		d.disp.DestroyPipeline(d.logical, r.pipeline, nil)
	}
	if r.pipeLayout != 0 {
		d.disp.DestroyPipelineLayout(d.logical, r.pipeLayout, nil)
	}
	if r.descPool != 0 {
		d.disp.DestroyDescriptorPool(d.logical, r.descPool, nil)
	}
	if r.setLayout != 0 {
		d.disp.DestroyDescriptorSetLayout(d.logical, r.setLayout, nil)
	}
	if r.sampler != 0 {
		d.disp.DestroySampler(d.logical, r.sampler, nil)
	}
	if r.atlasView != 0 {
		d.disp.DestroyImageView(d.logical, r.atlasView, nil)
	}
	if r.atlasImg != 0 {
		d.disp.DestroyImage(d.logical, r.atlasImg, nil)
	}
	if r.atlasMem != 0 {
		d.disp.FreeMemory(d.logical, r.atlasMem, nil)
	}
}
