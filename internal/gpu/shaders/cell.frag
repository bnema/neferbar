#version 450

layout(set = 0, binding = 0) uniform sampler2D atlas;

layout(location = 0) in vec2 vUV;
layout(location = 1) flat in vec4 vFG;
layout(location = 2) flat in vec4 vBG;
layout(location = 0) out vec4 outColor;

void main() {
    float coverage = texture(atlas, vUV).r;
    outColor = vec4(mix(vBG.rgb, vFG.rgb, coverage), 1.0);
}
