import AppKit
import ImageIO
import SwiftUI

enum Theme {
    static let blue = Color(red: 0x0A / 255, green: 0x7C / 255, blue: 1)
    static let purple = Color(red: 0xA0 / 255, green: 0x33 / 255, blue: 1)
    /// Nền xám nhạt: #F0F0F0 (light) / #303030 (dark).
    static let gray = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
            ? NSColor(white: 0x30 / 255.0, alpha: 1) : NSColor(white: 0xF0 / 255.0, alpha: 1)
    })
    static let outgoing = LinearGradient(colors: [blue, purple], startPoint: .top, endPoint: .bottom)
}

/// Nạp ảnh bất đồng bộ + downsample, chỉ cache trong bộ nhớ (NSCache, URLSession ephemeral không có cache đĩa).
final class ImageLoader: @unchecked Sendable {
    static let shared = ImageLoader()
    private let cache = NSCache<NSString, NSImage>()
    private let session: URLSession

    private init() {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.urlCache = URLCache(memoryCapacity: 20 << 20, diskCapacity: 0)
        cfg.requestCachePolicy = .returnCacheDataElseLoad
        session = URLSession(configuration: cfg)
        cache.totalCostLimit = 64 << 20
    }

    /// `path` là file local (tin mình gửi), `url` là ảnh remote. `maxPixel` là cạnh dài tối đa.
    func image(path: String?, url: String?, maxPixel: Int) async -> NSImage? {
        guard let key = path ?? url else { return nil }
        let cacheKey = "\(key)@\(maxPixel)" as NSString
        if let hit = cache.object(forKey: cacheKey) { return hit }
        var data: Data?
        if let path, FileManager.default.fileExists(atPath: path) {
            data = try? Data(contentsOf: URL(fileURLWithPath: path), options: .mappedIfSafe)
        } else if let url, let u = URL(string: url) {
            data = try? await session.data(from: u).0
        }
        guard let data, let img = await Task.detached(priority: .utility, operation: { Self.downsample(data, maxPixel: maxPixel) }).value
        else { return nil }
        cache.setObject(img, forKey: cacheKey, cost: Int(img.size.width * img.size.height * 4))
        return img
    }

    private static func downsample(_ data: Data, maxPixel: Int) -> NSImage? {
        guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        let opts: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixel,
        ]
        guard let cg = CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary) else { return nil }
        return NSImage(cgImage: cg, size: NSSize(width: cg.width, height: cg.height))
    }
}

/// Ảnh nạp bất đồng bộ (không đọc file trong body).
struct AsyncThumb<Placeholder: View>: View {
    let path: String?
    let url: String?
    let maxPixel: Int
    @ViewBuilder let placeholder: () -> Placeholder
    @State private var image: NSImage?

    var body: some View {
        Group {
            if let image { Image(nsImage: image).resizable().scaledToFill() } else { placeholder() }
        }
        .task(id: "\(path ?? "")|\(url ?? "")") {
            image = await ImageLoader.shared.image(path: path, url: url, maxPixel: maxPixel)
        }
    }
}
