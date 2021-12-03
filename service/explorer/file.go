package explorer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cloudreve/Cloudreve/v3/pkg/util"

	model "github.com/cloudreve/Cloudreve/v3/models"
	"github.com/cloudreve/Cloudreve/v3/pkg/cache"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/driver/local"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/fsctx"
	"github.com/cloudreve/Cloudreve/v3/pkg/serializer"
	"github.com/gin-gonic/gin"
)

// SingleFileService 对单文件进行操作的五福，path为文件完整路径
type SingleFileService struct {
	Path string `uri:"path" json:"path" binding:"required,min=1,max=65535"`
}

// FileIDService 通过文件ID对文件进行操作的服务
type FileIDService struct {
}

// FileAnonymousGetService 匿名（外链）获取文件服务
type FileAnonymousGetService struct {
	ID   uint   `uri:"id" binding:"required,min=1"`
	Name string `uri:"name" binding:"required"`
}

// DownloadService 文件下載服务
type DownloadService struct {
	ID string `uri:"id" binding:"required"`
}

// New 创建新文件
func (service *SingleFileService) Create(c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, fsctx.DisableOverwrite, true)

	// 给文件系统分配钩子
	fs.Use("BeforeUpload", filesystem.HookValidateFile)
	fs.Use("AfterUpload", filesystem.GenericAfterUpload)

	// 上传空文件
	err = fs.Upload(ctx, local.FileStream{
		File:        ioutil.NopCloser(strings.NewReader("")),
		Size:        0,
		VirtualPath: path.Dir(service.Path),
		Name:        path.Base(service.Path),
	})
	if err != nil {
		return serializer.Err(serializer.CodeUploadFailed, err.Error(), err)
	}

	return serializer.Response{
		Code: 0,
	}
}

// List 列出从机上的文件
func (service *SlaveListService) List(c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewAnonymousFileSystem()
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	objects, err := fs.Handler.List(context.Background(), service.Path, service.Recursive)
	if err != nil {
		return serializer.Err(serializer.CodeIOFailed, "无法列取文件", err)
	}

	res, _ := json.Marshal(objects)
	return serializer.Response{Data: string(res)}
}

// DownloadArchived 下載已打包的多文件
func (service *DownloadService) DownloadArchived(ctx context.Context, c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 查找打包的临时文件
	zipPath, exist := cache.Get("archive_" + service.ID)
	if !exist {
		return serializer.Err(404, "归档文件不存在", nil)
	}

	// 获取文件流
	rs, err := fs.GetPhysicalFileContent(ctx, zipPath.(string))
	defer rs.Close()
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	if fs.User.Group.OptionsSerialized.OneTimeDownload {
		// 清理资源，删除临时文件
		_ = cache.Deletes([]string{service.ID}, "archive_")
	}

	c.Header("Content-Disposition", "attachment;")
	c.Header("Content-Type", "application/zip")
	http.ServeContent(c.Writer, c.Request, "", time.Now(), rs)

	return serializer.Response{
		Code: 0,
	}

}

// Download 签名的匿名文件下载
func (service *FileAnonymousGetService) Download(ctx context.Context, c *gin.Context) serializer.Response {
	fs, err := filesystem.NewAnonymousFileSystem()
	if err != nil {
		return serializer.Err(serializer.CodeGroupNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 查找文件
	err = fs.SetTargetFileByIDs([]uint{service.ID})
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	// 获取文件流
	rs, err := fs.GetDownloadContent(ctx, 0)
	defer rs.Close()
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	// 发送文件
	http.ServeContent(c.Writer, c.Request, service.Name, fs.FileTarget[0].UpdatedAt, rs)

	return serializer.Response{
		Code: 0,
	}
}

// Source 重定向到文件的有效原始链接
func (service *FileAnonymousGetService) Source(ctx context.Context, c *gin.Context) serializer.Response {
	fs, err := filesystem.NewAnonymousFileSystem()
	if err != nil {
		return serializer.Err(serializer.CodeGroupNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 查找文件
	err = fs.SetTargetFileByIDs([]uint{service.ID})
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	// 获取文件流
	res, err := fs.SignURL(ctx, &fs.FileTarget[0],
		int64(model.GetIntSetting("preview_timeout", 60)), false)
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	return serializer.Response{
		Code: -302,
		Data: res,
	}
}

// CreateDocPreviewSession 创建DOC文件预览会话，返回预览地址
func (service *FileIDService) CreateDocPreviewSession(ctx context.Context, c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 获取对象id
	objectID, _ := c.Get("object_id")

	// 如果上下文中已有File对象，则重设目标
	if file, ok := ctx.Value(fsctx.FileModelCtx).(*model.File); ok {
		fs.SetTargetFile(&[]model.File{*file})
		objectID = uint(0)
	}

	// 如果上下文中已有Folder对象，则重设根目录
	if folder, ok := ctx.Value(fsctx.FolderModelCtx).(*model.Folder); ok {
		fs.Root = folder
		path := ctx.Value(fsctx.PathCtx).(string)
		err := fs.ResetFileIfNotExist(ctx, path)
		if err != nil {
			return serializer.Err(serializer.CodeNotFound, err.Error(), err)
		}
		objectID = uint(0)
	}

	// 获取文件临时下载地址
	downloadURL, err := fs.GetDownloadURL(ctx, objectID.(uint), "doc_preview_timeout")
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	// 生成最终的预览器地址
	srcB64 := base64.StdEncoding.EncodeToString([]byte(downloadURL))
	srcEncoded := url.QueryEscape(downloadURL)
	srcB64Encoded := url.QueryEscape(srcB64)
	return serializer.Response{
		Code: 0,
		Data: util.Replace(map[string]string{
			"{$src}":    srcEncoded,
			"{$srcB64}": srcB64Encoded,
		}, model.GetSettingByName("office_preview_service")),
	}
}

// CreateDownloadSession 创建下载会话，获取下载URL
func (service *FileIDService) CreateDownloadSession(ctx context.Context, c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 获取对象id
	objectID, _ := c.Get("object_id")

	// 获取下载地址
	downloadURL, err := fs.GetDownloadURL(ctx, objectID.(uint), "download_timeout")
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	return serializer.Response{
		Code: 0,
		Data: downloadURL,
	}
}

// Download 通过签名URL的文件下载，无需登录
func (service *DownloadService) Download(ctx context.Context, c *gin.Context) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 查找打包的临时文件
	file, exist := cache.Get("download_" + service.ID)
	if !exist {
		return serializer.Err(404, "文件下载会话不存在", nil)
	}
	fs.FileTarget = []model.File{file.(model.File)}

	// 开始处理下载
	ctx = context.WithValue(ctx, fsctx.GinCtx, c)
	rs, err := fs.GetDownloadContent(ctx, 0)
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}
	defer rs.Close()

	// 设置文件名
	c.Header("Content-Disposition", "attachment; filename=\""+url.PathEscape(fs.FileTarget[0].Name)+"\"")

	if fs.User.Group.OptionsSerialized.OneTimeDownload {
		// 清理资源，删除临时文件
		_ = cache.Deletes([]string{service.ID}, "download_")
	}

	// 发送文件
	http.ServeContent(c.Writer, c.Request, fs.FileTarget[0].Name, fs.FileTarget[0].UpdatedAt, rs)

	return serializer.Response{
		Code: 0,
	}
}

// PreviewContent 预览文件，需要登录会话, isText - 是否为文本文件，文本文件会
// 强制经由服务端中转
func (service *FileIDService) PreviewContent(ctx context.Context, c *gin.Context, isText bool) serializer.Response {
	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	defer fs.Recycle()

	// 获取对象id
	objectID, _ := c.Get("object_id")

	// 如果上下文中已有File对象，则重设目标
	if file, ok := ctx.Value(fsctx.FileModelCtx).(*model.File); ok {
		fs.SetTargetFile(&[]model.File{*file})
		objectID = uint(0)
	}

	// 如果上下文中已有Folder对象，则重设根目录
	if folder, ok := ctx.Value(fsctx.FolderModelCtx).(*model.Folder); ok {
		fs.Root = folder
		path := ctx.Value(fsctx.PathCtx).(string)
		err := fs.ResetFileIfNotExist(ctx, path)
		if err != nil {
			return serializer.Err(serializer.CodeNotFound, err.Error(), err)
		}
		objectID = uint(0)
	}

	// 获取文件预览响应
	resp, err := fs.Preview(ctx, objectID.(uint), isText)
	if err != nil {
		return serializer.Err(serializer.CodeNotSet, err.Error(), err)
	}

	// 重定向到文件源
	if resp.Redirect {
		c.Header("Cache-Control", fmt.Sprintf("max-age=%d", resp.MaxAge))
		return serializer.Response{
			Code: -301,
			Data: resp.URL,
		}
	}

	// 直接返回文件内容
	defer resp.Content.Close()

	if isText {
		c.Header("Cache-Control", "no-cache")
	}

	http.ServeContent(c.Writer, c.Request, fs.FileTarget[0].Name, fs.FileTarget[0].UpdatedAt, resp.Content)

	return serializer.Response{
		Code: 0,
	}
}

// PutContent 更新文件内容
func (service *FileIDService) PutContent(ctx context.Context, c *gin.Context) serializer.Response {
	// 创建上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 取得文件大小
	fileSize, err := strconv.ParseUint(c.Request.Header.Get("Content-Length"), 10, 64)
	if err != nil {

		return serializer.ParamErr("无法解析文件尺寸", err)
	}

	fileData := local.FileStream{
		MIMEType: c.Request.Header.Get("Content-Type"),
		File:     c.Request.Body,
		Size:     fileSize,
	}

	// 创建文件系统
	fs, err := filesystem.NewFileSystemFromContext(c)
	if err != nil {
		return serializer.Err(serializer.CodePolicyNotAllowed, err.Error(), err)
	}
	uploadCtx := context.WithValue(ctx, fsctx.GinCtx, c)

	// 取得现有文件
	fileID, _ := c.Get("object_id")
	originFile, _ := model.GetFilesByIDs([]uint{fileID.(uint)}, fs.User.ID)
	if len(originFile) == 0 {
		return serializer.Err(404, "文件不存在", nil)
	}
	fileData.Name = originFile[0].Name

	// 检查此文件是否有软链接
	fileList, err := model.RemoveFilesWithSoftLinks([]model.File{originFile[0]})
	if err == nil && len(fileList) == 0 {
		// 如果包含软连接，应重新生成新文件副本，并更新source_name
		originFile[0].SourceName = fs.GenerateSavePath(uploadCtx, fileData)
		fs.Use("AfterUpload", filesystem.HookUpdateSourceName)
		fs.Use("AfterUploadCanceled", filesystem.HookUpdateSourceName)
		fs.Use("AfterValidateFailed", filesystem.HookUpdateSourceName)
	}

	// 给文件系统分配钩子
	fs.Use("BeforeUpload", filesystem.HookResetPolicy)
	fs.Use("BeforeUpload", filesystem.HookValidateFile)
	fs.Use("BeforeUpload", filesystem.HookChangeCapacity)
	fs.Use("AfterUploadCanceled", filesystem.HookCleanFileContent)
	fs.Use("AfterUploadCanceled", filesystem.HookClearFileSize)
	fs.Use("AfterUploadCanceled", filesystem.HookGiveBackCapacity)
	fs.Use("AfterUpload", filesystem.GenericAfterUpdate)
	fs.Use("AfterValidateFailed", filesystem.HookCleanFileContent)
	fs.Use("AfterValidateFailed", filesystem.HookClearFileSize)
	fs.Use("AfterValidateFailed", filesystem.HookGiveBackCapacity)

	// 执行上传
	uploadCtx = context.WithValue(uploadCtx, fsctx.FileModelCtx, originFile[0])
	err = fs.Upload(uploadCtx, fileData)
	if err != nil {
		return serializer.Err(serializer.CodeUploadFailed, err.Error(), err)
	}

	return serializer.Response{
		Code: 0,
	}
}
