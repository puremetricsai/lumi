import AppKit

/// MenuBarGlyph turns Lumi's mark into a menu bar template image.
///
/// The shipped logo is a cursive white "l" painted on a solid amber disc, and
/// both are fully opaque. A template image uses only the alpha channel, so
/// handing that file to the menu bar draws a featureless filled circle — the
/// letter disappears because it is opaque white, not a hole.
///
/// So the letter is knocked out here, by saturation. The disc is a saturated
/// amber and the letter is pure white, which has no saturation at all, and
/// every antialiased pixel between them falls smoothly in between — which is
/// what keeps the edges from going jagged. Deriving the mask at runtime also
/// means the repository keeps one logo file rather than a hand-made second copy
/// that could drift from it.
enum MenuBarGlyph {
    /// The menu bar's usable height leaves about 18pt for an icon.
    static let side = 18

    static func make(from url: URL) -> NSImage? {
        guard let source = NSImage(contentsOf: url),
              let cgImage = source.cgImage(forProposedRect: nil, context: nil, hints: nil) else {
            return nil
        }
        // Rendered at 2x so the mark stays crisp on a Retina display.
        let pixels = side * 2
        guard let context = CGContext(
            data: nil, width: pixels, height: pixels,
            bitsPerComponent: 8, bytesPerRow: pixels * 4,
            space: CGColorSpaceCreateDeviceRGB(),
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else {
            return nil
        }
        context.interpolationQuality = .high
        context.draw(cgImage, in: CGRect(x: 0, y: 0, width: pixels, height: pixels))
        guard let data = context.data else { return nil }

        let buffer = data.bindMemory(to: UInt8.self, capacity: pixels * pixels * 4)
        for index in stride(from: 0, to: pixels * pixels * 4, by: 4) {
            let alpha = Double(buffer[index + 3]) / 255
            guard alpha > 0 else { continue }
            // Un-premultiply before measuring colour, or a partly transparent
            // amber pixel reads as darker than it is and keeps too much alpha.
            let r = Double(buffer[index]) / 255 / alpha
            let g = Double(buffer[index + 1]) / 255 / alpha
            let b = Double(buffer[index + 2]) / 255 / alpha
            let highest = max(r, g, b)
            let saturation = highest > 0 ? (highest - min(r, g, b)) / highest : 0
            let kept = alpha * min(1, saturation)
            // Template images read alpha only, but the colour is set to black
            // so the bitmap is also correct if it is ever drawn untinted.
            let value = UInt8(clamping: Int(kept * 255))
            buffer[index] = 0
            buffer[index + 1] = 0
            buffer[index + 2] = 0
            buffer[index + 3] = value
        }
        guard let masked = context.makeImage() else { return nil }
        let image = NSImage(cgImage: masked, size: NSSize(width: side, height: side))
        image.isTemplate = true
        return image
    }
}
