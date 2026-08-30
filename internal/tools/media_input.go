package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

// inputImageRawBudget 是我们会打包进 data URI 供 img2img 使用的最大 JPEG 字节数。
// Agnes 接受总长最多约 80KB 的 data URI；base64 会把负载放大约 33%，
// 因此把原始 JPEG 限制在 50KB，能让最终 URI 舒适地保持在该阈值以下。
const inputImageRawBudget = 50 * 1024

// prepareInputImage 为上游 API 规范化一条 input_images 条目。
//
//   - http(s):// URL 原样通过——提供方自行抓取，对 URL 也没有可执行的
//     有用大小限制。
//   - data: URI 和本地文件路径会被解码、缩小并重新编码为 JPEG，
//     直到符合 inputImageRawBudget，然后作为 data URI 返回。
//
// workDir 用于解析相对路径。文件不可读或图像无法解码时返回 ("", err)。
func prepareInputImage(in, workDir string) (string, error) {
	if in == "" {
		return "", errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return in, nil
	}

	var raw []byte
	switch {
	case strings.HasPrefix(low, "data:"):
		b, err := decodeDataURI(in)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		path := in
		if !filepath.IsAbs(path) && workDir != "" {
			path = filepath.Join(workDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read input image %q: %w", in, err)
		}
		raw = b
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode input image: %w", err)
	}

	jpegBytes, err := shrinkToBudget(img, inputImageRawBudget)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes), nil
}

// shrinkToBudget 将 img 编码为 JPEG，先降低质量再减半尺寸，直到编码后的
// 字节数 <= budget。返回我们设法产出的最小编码形式；只有当连 8x8 缩略图
// 都放不下时才报错（那意味着 budget 小得离谱）。
func shrinkToBudget(img image.Image, budget int) ([]byte, error) {
	qualities := []int{82, 65, 45, 28}
	for {
		for _, q := range qualities {
			buf := &bytes.Buffer{}
			if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: q}); err != nil {
				return nil, fmt.Errorf("encode jpeg: %w", err)
			}
			if buf.Len() <= budget {
				return buf.Bytes(), nil
			}
		}
		b := img.Bounds()
		w, h := b.Dx()/2, b.Dy()/2
		if w < 8 || h < 8 {
			return nil, fmt.Errorf("cannot shrink image under %d bytes", budget)
		}
		img = halve(img)
	}
}

// halve 用 2x2 盒子平均法把 img 缩小到一半宽、一半高。
// 盒子滤波开销小且能产生干净的降采样——由于结果要喂给 img2img 模型，
// 不需要精确重建。
func halve(src image.Image) image.Image {
	sb := src.Bounds()
	w, h := sb.Dx()/2, sb.Dy()/2
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx, sy := sb.Min.X+x*2, sb.Min.Y+y*2
			r1, g1, b1, a1 := src.At(sx, sy).RGBA()
			r2, g2, b2, a2 := src.At(sx+1, sy).RGBA()
			r3, g3, b3, a3 := src.At(sx, sy+1).RGBA()
			r4, g4, b4, a4 := src.At(sx+1, sy+1).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r1 + r2 + r3 + r4) >> 10),
				G: uint8((g1 + g2 + g3 + g4) >> 10),
				B: uint8((b1 + b2 + b3 + b4) >> 10),
				A: uint8((a1 + a2 + a3 + a4) >> 10),
			})
		}
	}
	return dst
}

// validateVideoInputURL 强制 generate_video 的输入图像必须是公开的 http(s) URL。
// Agnes 视频端点只接受 URL——本地路径和 data URI 会触发其 base64 / fetch 处理，
// 任务随后以令人困惑的 "Invalid image" 错误异步失败，
// 这过去在调用方看来像是泛化的 "parse error" 重试循环。
func validateVideoInputURL(in string) error {
	if in == "" {
		return errors.New("empty input image")
	}
	low := strings.ToLower(in)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil
	}
	if strings.HasPrefix(low, "data:") {
		return fmt.Errorf("video input_images must be public http(s) URLs; data URIs are not accepted by the upstream API")
	}
	return fmt.Errorf("video input_images must be public http(s) URLs (got %q); upload the image to a reachable URL first", in)
}

// decodeDataURI 从 "data:[mime];base64,..." 字符串中返回原始字节。
// 非 base64 的 data URI 会被拒绝，因为 API 只往返 base64。
func decodeDataURI(s string) ([]byte, error) {
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, errors.New("data URI missing comma separator")
	}
	header := s[:comma]
	if !strings.Contains(header, ";base64") {
		return nil, errors.New("data URI must be base64-encoded")
	}
	return base64.StdEncoding.DecodeString(s[comma+1:])
}
