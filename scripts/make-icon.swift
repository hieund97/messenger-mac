#!/usr/bin/env swift
// Vẽ icon MacMess (1024 master) rồi xuất iconset + App/Resources/MacMess.icns.
// Chạy: swift scripts/make-icon.swift [đường-dẫn-preview-512.png]
import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

let root = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().deletingLastPathComponent()
let cs = CGColorSpace(name: CGColorSpace.sRGB)!

func rgb(_ hex: UInt32, _ a: CGFloat = 1) -> CGColor {
    CGColor(colorSpace: cs, components: [
        CGFloat((hex >> 16) & 0xFF) / 255, CGFloat((hex >> 8) & 0xFF) / 255, CGFloat(hex & 0xFF) / 255, a,
    ])!
}

func makeContext(_ size: Int) -> CGContext {
    CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: 0, space: cs,
              bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
}

// Squircle (superellipse |x|^n + |y|^n = 1) cho bo góc liên tục kiểu macOS.
func squircle(_ r: CGRect, n: CGFloat = 4.6) -> CGPath {
    let p = CGMutablePath()
    let steps = 360
    for i in 0...steps {
        let t = CGFloat(i) / CGFloat(steps) * 2 * .pi
        let c = cos(t), s = sin(t)
        let x = r.midX + r.width / 2 * (c < 0 ? -1 : 1) * pow(abs(c), 2 / n)
        let y = r.midY + r.height / 2 * (s < 0 ? -1 : 1) * pow(abs(s), 2 / n)
        i == 0 ? p.move(to: CGPoint(x: x, y: y)) : p.addLine(to: CGPoint(x: x, y: y))
    }
    p.closeSubpath()
    return p
}

func drawMaster() -> CGImage {
    let S: CGFloat = 1024
    let ctx = makeContext(1024)
    let body = CGRect(x: 100, y: 100, width: 824, height: 824)
    let shape = squircle(body)
    let blue = rgb(0x0A7CFF), purple = rgb(0x8E3BFF)
    let grad = CGGradient(colorsSpace: cs, colors: [blue, purple] as CFArray, locations: [0, 1])!

    // Bóng đổ phía dưới
    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -14), blur: 28, color: rgb(0x000000, 0.35))
    ctx.addPath(shape)
    ctx.setFillColor(purple)
    ctx.fillPath()
    ctx.restoreGState()

    // Nền gradient chéo (trên trái -> dưới phải)
    ctx.saveGState()
    ctx.addPath(shape)
    ctx.clip()
    ctx.drawLinearGradient(grad, start: CGPoint(x: body.minX, y: body.maxY),
                           end: CGPoint(x: body.maxX, y: body.minY), options: [])
    // Highlight nhẹ phía trên
    let hl = CGGradient(colorsSpace: cs, colors: [rgb(0xFFFFFF, 0.28), rgb(0xFFFFFF, 0)] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(hl, start: CGPoint(x: 0, y: body.maxY), end: CGPoint(x: 0, y: body.midY + 60), options: [])
    ctx.restoreGState()

    // Bong bóng chat: oval dẹt + đuôi nhỏ góc dưới trái
    let bubble = CGMutablePath()
    bubble.addEllipse(in: CGRect(x: 512 - 285, y: 545 - 215, width: 570, height: 430))
    let tail = CGMutablePath()
    tail.move(to: CGPoint(x: 330, y: 440))
    tail.addLine(to: CGPoint(x: 262, y: 322))
    tail.addQuadCurve(to: CGPoint(x: 290, y: 312), control: CGPoint(x: 262, y: 312))
    tail.addLine(to: CGPoint(x: 470, y: 350))
    tail.closeSubpath()
    bubble.addPath(tail)

    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -10), blur: 22, color: rgb(0x2A0A7A, 0.35))
    ctx.addPath(bubble)
    ctx.setFillColor(rgb(0xFFFFFF))
    ctx.fillPath()
    ctx.restoreGState()

    // 3 chấm gradient xanh-tím
    let dots = CGMutablePath()
    for dx: CGFloat in [-120, 0, 120] {
        dots.addEllipse(in: CGRect(x: 512 + dx - 42, y: 545 - 42, width: 84, height: 84))
    }
    ctx.saveGState()
    ctx.addPath(dots)
    ctx.clip()
    ctx.drawLinearGradient(grad, start: CGPoint(x: 390, y: 600), end: CGPoint(x: 634, y: 490), options: [.drawsBeforeStartLocation, .drawsAfterEndLocation])
    ctx.restoreGState()
    _ = S
    return ctx.makeImage()!
}

func resized(_ img: CGImage, _ size: Int) -> CGImage {
    let ctx = makeContext(size)
    ctx.interpolationQuality = .high
    ctx.draw(img, in: CGRect(x: 0, y: 0, width: size, height: size))
    return ctx.makeImage()!
}

func writePNG(_ img: CGImage, _ url: URL) {
    let d = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil)!
    CGImageDestinationAddImage(d, img, nil)
    guard CGImageDestinationFinalize(d) else { fatalError("Không ghi được \(url.path)") }
}

let master = drawMaster()
let fm = FileManager.default
let tmp = fm.temporaryDirectory.appendingPathComponent("MacMess-\(UUID().uuidString)")
let iconset = tmp.appendingPathComponent("MacMess.iconset")
try fm.createDirectory(at: iconset, withIntermediateDirectories: true)
defer { try? fm.removeItem(at: tmp) }

for base in [16, 32, 128, 256, 512] {
    writePNG(resized(master, base), iconset.appendingPathComponent("icon_\(base)x\(base).png"))
    writePNG(resized(master, base * 2), iconset.appendingPathComponent("icon_\(base)x\(base)@2x.png"))
}
// Các cỡ bổ sung theo yêu cầu: 64 và 1024
writePNG(resized(master, 64), iconset.appendingPathComponent("icon_64x64.png"))
writePNG(master, tmp.appendingPathComponent("master-1024.png"))

let resources = root.appendingPathComponent("App/Resources")
try fm.createDirectory(at: resources, withIntermediateDirectories: true)
let icns = resources.appendingPathComponent("MacMess.icns")
let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset.path, "-o", icns.path]
try p.run()
p.waitUntilExit()
guard p.terminationStatus == 0 else { fatalError("iconutil lỗi") }
print("Wrote \(icns.path)")

if CommandLine.arguments.count > 1 {
    let out = URL(fileURLWithPath: CommandLine.arguments[1])
    writePNG(resized(master, 512), out)
    writePNG(resized(master, 32), out.deletingLastPathComponent().appendingPathComponent("icon-preview-32.png"))
    print("Preview \(out.path)")
}
