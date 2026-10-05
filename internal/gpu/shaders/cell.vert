#version 450

// One instance per terminal cell (or per double-width cell pair).
layout(location = 0) in vec4 cell; // x, y, w, h in pixels
layout(location = 1) in vec4 uv;   // atlas u0, v0, u1, v1
layout(location = 2) in vec4 fg;
layout(location = 3) in vec4 bg;

layout(push_constant) uniform Screen { vec2 size; } screen;

layout(location = 0) out vec2 vUV;
layout(location = 1) flat out vec4 vFG;
layout(location = 2) flat out vec4 vBG;

void main() {
    vec2 corner[6] = vec2[6](
        vec2(0.0, 0.0), vec2(1.0, 0.0), vec2(0.0, 1.0),
        vec2(0.0, 1.0), vec2(1.0, 0.0), vec2(1.0, 1.0));
    vec2 q = corner[gl_VertexIndex];
    vec2 xy = cell.xy + q * cell.zw;
    gl_Position = vec4(xy * 2.0 / screen.size - 1.0, 0.0, 1.0);
    vUV = mix(uv.xy, uv.zw, q);
    vFG = fg;
    vBG = bg;
}
