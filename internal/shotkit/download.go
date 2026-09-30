package shotkit

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// httpDownload 下载到指定路径（调用方负责原子改名）。
func httpDownload(url, path string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("下载失败 HTTP %d: %s", resp.StatusCode, url)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n < 1024 {
		return fmt.Errorf("下载内容过小（%d 字节），疑似错误响应", n)
	}
	return nil
}

// ReadAsDataURL 读取本地图片并编码为 data URL，用于把首帧图传给图生视频接口。
// 智谱的 image_url 既接受 URL 也接受 base64，用 data URL 可以避免先上传再引用。
func ReadAsDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mime := "image/png"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".webp":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// NormalizeFrame 把生成的首帧图规整成视频接口要求的尺寸。
//
// 为什么需要：文生图返回的尺寸常与视频接口期望的不一致（且部分接口要求 16 的整数倍），
// 尺寸不符会在提交阶段直接报错，白白浪费一次出图费用。这里本地缩放，不花一分钱。
func NormalizeFrame(src, dst string, targetW, targetH int) error {
	if targetW <= 0 || targetH <= 0 {
		return fmt.Errorf("目标尺寸非法: %dx%d", targetW, targetH)
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("解码首帧失败: %w", err)
	}
	b := img.Bounds()
	if b.Dx() == targetW && b.Dy() == targetH {
		return nil // 已经符合要求，不重复编码（避免二次压缩损失）
	}
	// 最近邻+双线性混合过于复杂，这里用面积平均做一次高质量缩放
	out := resizeBilinear(img, targetW, targetH)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// resizeBilinear 双线性缩放（够用于首帧预处理；生成质量由模型决定）。
func resizeBilinear(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return dst
	}
	for y := 0; y < h; y++ {
		fy := (float64(y) + 0.5) * float64(sh) / float64(h)
		y0 := int(fy)
		if y0 >= sh {
			y0 = sh - 1
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		dy := fy - float64(y0)
		for x := 0; x < w; x++ {
			fx := (float64(x) + 0.5) * float64(sw) / float64(w)
			x0 := int(fx)
			if x0 >= sw {
				x0 = sw - 1
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			dx := fx - float64(x0)
			r00, g00, b00, a00 := src.At(sb.Min.X+x0, sb.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := src.At(sb.Min.X+x1, sb.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := src.At(sb.Min.X+x0, sb.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := src.At(sb.Min.X+x1, sb.Min.Y+y1).RGBA()
			lerp := func(v00, v10, v01, v11 uint32) uint8 {
				top := float64(v00)*(1-dx) + float64(v10)*dx
				bot := float64(v01)*(1-dx) + float64(v11)*dx
				v := top*(1-dy) + bot*dy
				return uint8(v / 257)
			}
			_ = a00
			_ = a10
			_ = a01
			_ = a11
			dst.SetRGBA(x, y, color.RGBA{
				R: lerp(r00, r10, r01, r11),
				G: lerp(g00, g10, g01, g11),
				B: lerp(b00, b10, b01, b11),
				A: 255,
			})
		}
	}
	return dst
}
