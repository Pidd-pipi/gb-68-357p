package controllers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/logger"
	"irrigation/pkg/response"
)

type DeviceController struct {
	deviceService  *services.DeviceService
	commandService *services.CommandService
}

func NewDeviceController() *DeviceController {
	return &DeviceController{
		deviceService:  services.NewDeviceService(),
		commandService: services.NewCommandService(),
	}
}

// ListDevices godoc
// @Summary 获取设备列表
// @Description 获取所有设备，支持按区域、类型、状态筛选
// @Tags 设备管理
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param type query string false "设备类型"
// @Param status query string false "设备状态"
// @Success 200 {array} models.Device
// @Router /api/devices [get]
func (c *DeviceController) List(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var deviceType *string
	if t := ctx.Query("type"); t != "" {
		deviceType = &t
	}

	var status *string
	if s := ctx.Query("status"); s != "" {
		status = &s
	}

	devices, err := c.deviceService.ListDevices(zoneID, deviceType, status)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}
	response.Success(ctx, devices)
}

// GetDevice godoc
// @Summary 获取设备详情
// @Description 根据ID获取设备详情
// @Tags 设备管理
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "设备ID"
// @Success 200 {object} models.Device
// @Router /api/devices/{id} [get]
func (c *DeviceController) Get(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)
	device, err := c.deviceService.GetDeviceByID(uint(id))
	if err != nil {
		response.NotFound(ctx, "Device not found")
		return
	}
	response.Success(ctx, device)
}

// CreateDevice godoc
// @Summary 创建设备
// @Description 注册新设备
// @Tags 设备管理
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param request body models.Device true "设备信息"
// @Success 201 {object} models.Device
// @Router /api/devices [post]
func (c *DeviceController) Create(ctx *gin.Context) {
	var device models.Device
	if err := ctx.ShouldBindJSON(&device); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.deviceService.CreateDevice(&device); err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Created(ctx, device)
}

// UpdateDevice godoc
// @Summary 更新设备
// @Description 更新设备信息
// @Tags 设备管理
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param id path int true "设备ID"
// @Param request body map[string]interface{} true "更新信息"
// @Success 200 {object} response.Response
// @Router /api/devices/{id} [put]
func (c *DeviceController) Update(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	var updates map[string]interface{}
	if err := ctx.ShouldBindJSON(&updates); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.deviceService.UpdateDevice(uint(id), updates); err != nil {
		response.NotFound(ctx, err.Error())
		return
	}

	response.Success(ctx, nil)
}

// DeleteDevice godoc
// @Summary 删除设备
// @Description 删除设备
// @Tags 设备管理
// @Security ApiKeyAuth
// @Produce json
// @Param id path int true "设备ID"
// @Success 200 {object} response.Response
// @Router /api/devices/{id} [delete]
func (c *DeviceController) Delete(ctx *gin.Context) {
	id, _ := strconv.ParseUint(ctx.Param("id"), 10, 32)

	if err := c.deviceService.DeleteDevice(uint(id)); err != nil {
		response.NotFound(ctx, err.Error())
		return
	}

	response.Success(ctx, nil)
}

// heartbeatRequest 设备心跳可携带命令回执
type heartbeatRequest struct {
	// AckedCommands 设备已收到（进入执行）的命令编号列表
	AckedCommands []string `json:"acked_commands"`
	// Results 设备回报的命令执行结果
	Results []struct {
		CommandNo    string   `json:"command_no" binding:"required"`
		Success      bool     `json:"success"`
		WaterUsage   *float64 `json:"water_usage"`
		ErrorMessage *string  `json:"error_message"`
	} `json:"results"`
}

// Heartbeat godoc
// @Summary 设备心跳
// @Description 设备上报心跳，可在 body 中携带命令回执；响应返回待执行命令
// @Tags 设备管理
// @Accept json
// @Produce json
// @Param serial path string true "设备序列号"
// @Param body body heartbeatRequest false "命令回执（可选）"
// @Success 200 {object} response.Response
// @Router /api/devices/{serial}/heartbeat [post]
func (c *DeviceController) Heartbeat(ctx *gin.Context) {
	serial := ctx.Param("serial")

	// body 可选：不带 body 的旧设备仍可只上报心跳
	var req heartbeatRequest
	if ctx.Request.ContentLength > 0 {
		if err := ctx.ShouldBindJSON(&req); err != nil {
			response.BadRequest(ctx, "Invalid request body")
			return
		}
	}

	device, err := c.deviceService.Heartbeat(serial)
	if err != nil {
		if errors.Is(err, services.ErrDeviceNotFound) {
			response.NotFound(ctx, "Device not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	now := time.Now()

	// 先处理"已收到命令"回执：pending -> executing；重复/过期/串设备自动忽略
	for _, commandNo := range req.AckedCommands {
		if _, err := c.commandService.AckCommand(serial, commandNo, now); err != nil {
			logger.Error("Failed to process command ack",
				zap.String("serial", serial), zap.String("command_no", commandNo), zap.Error(err))
			response.InternalServerError(ctx, err.Error())
			return
		}
	}

	// 再处理执行结果回报：executing -> success/failed
	for _, result := range req.Results {
		if _, err := c.commandService.ReportCommandResult(
			serial, result.CommandNo, result.Success,
			result.WaterUsage, result.ErrorMessage, now,
		); err != nil {
			logger.Error("Failed to process command result",
				zap.String("serial", serial), zap.String("command_no", result.CommandNo), zap.Error(err))
			response.InternalServerError(ctx, err.Error())
			return
		}
	}

	// 返回仍在等待确认的命令，设备据此拉取待执行指令
	pending, err := c.commandService.ListPendingForDevice(device.ID, now)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, gin.H{
		"device_id":        device.ID,
		"server_time":      now,
		"pending_commands": pending,
	})
}
