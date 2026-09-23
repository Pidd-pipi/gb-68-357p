package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type IrrigationController struct {
	irrigationService *services.IrrigationService
	commandService    *services.CommandService
	zoneService       *services.ZoneService
	deviceService     *services.DeviceService
}

func NewIrrigationController() *IrrigationController {
	return &IrrigationController{
		irrigationService: services.NewIrrigationService(),
		commandService:    services.NewCommandService(),
		zoneService:       services.NewZoneService(),
		deviceService:     services.NewDeviceService(),
	}
}

// ManualIrrigate godoc
// @Summary 手动灌溉
// @Description 触发手动灌溉，生成待确认命令并返回命令编号，设备回执后进入执行中
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Accept json
// @Produce json
// @Param zone_id body int true "区域ID"
// @Success 200 {object} models.IrrigationCommand
// @Router /api/irrigation/manual [post]
func (c *IrrigationController) ManualIrrigate(ctx *gin.Context) {
	var req struct {
		ZoneID uint `json:"zone_id" binding:"required"`
	}

	if err := ctx.ShouldBindJSON(&req); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if _, err := c.zoneService.GetZoneByID(req.ZoneID); err != nil {
		response.NotFound(ctx, "zone not found")
		return
	}

	valve, err := c.findOnlineValve(req.ZoneID)
	if err != nil {
		response.BadRequest(ctx, err.Error())
		return
	}

	log, err := c.irrigationService.StartIrrigation(nil, &req.ZoneID, models.TriggerTypeManual)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	cmd, err := c.commandService.CreateCommand(req.ZoneID, valve.ID, &log.ID)
	if err != nil {
		if err == services.ErrCommandNoConflict {
			response.Error(ctx, http.StatusConflict, "command number conflict, please retry")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, cmd)
}

// findOnlineValve 查找区域内在线的阀门设备；无阀门或阀门离线时明确拒绝
func (c *IrrigationController) findOnlineValve(zoneID uint) (*models.Device, error) {
	valveType := string(models.DeviceTypeValve)
	valves, err := c.deviceService.ListDevices(&zoneID, &valveType, nil)
	if err != nil {
		return nil, err
	}
	if len(valves) == 0 {
		return nil, errors.New("no valve device in this zone")
	}
	for i := range valves {
		if valves[i].Status == models.DeviceStatusOnline {
			return &valves[i], nil
		}
	}
	return nil, errors.New("valve device is offline, command rejected")
}

// GetCommand godoc
// @Summary 查询灌溉命令结果
// @Description 管理员按命令编号查询命令执行结果
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Produce json
// @Param command_no path string true "命令编号"
// @Success 200 {object} models.IrrigationCommand
// @Router /api/irrigation/commands/{command_no} [get]
func (c *IrrigationController) GetCommand(ctx *gin.Context) {
	commandNo := ctx.Param("command_no")

	cmd, err := c.commandService.GetCommandByNo(commandNo)
	if err != nil {
		if err == services.ErrCommandNotFound {
			response.NotFound(ctx, "command not found")
			return
		}
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, cmd)
}

// GetIrrigationHistory godoc
// @Summary 获取灌溉历史
// @Description 获取灌溉执行历史记录
// @Tags 灌溉执行
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Param limit query int false "返回数量限制" default(100)
// @Success 200 {array} models.IrrigationLog
// @Router /api/irrigation/history [get]
func (c *IrrigationController) GetHistory(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	limit := 100
	if limitStr := ctx.Query("limit"); limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	logs, err := c.irrigationService.GetIrrigationHistory(zoneID, startTime, endTime, limit)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, logs)
}

// GetWaterUsageStats godoc
// @Summary 获取用水量统计
// @Description 获取指定时间段的用水量统计
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param zone_id query int false "区域ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {object} services.WaterUsageStats
// @Router /api/statistics/water-usage [get]
func (c *IrrigationController) GetWaterUsageStats(ctx *gin.Context) {
	var zoneID *uint
	if zoneIDStr := ctx.Query("zone_id"); zoneIDStr != "" {
		id, _ := strconv.ParseUint(zoneIDStr, 10, 32)
		idUint := uint(id)
		zoneID = &idUint
	}

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	stats, err := c.irrigationService.GetWaterUsageStats(zoneID, startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, stats)
}

// GetZoneWaterUsage godoc
// @Summary 获取各区域用水量
// @Description 获取各区域的用水量分布
// @Tags 用水统计
// @Security ApiKeyAuth
// @Produce json
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Success 200 {array} services.ZoneWaterUsage
// @Router /api/statistics/zone-usage [get]
func (c *IrrigationController) GetZoneWaterUsage(ctx *gin.Context) {
	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	} else {
		startTime = time.Now().AddDate(0, 0, -7)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	} else {
		endTime = time.Now()
	}

	usage, err := c.irrigationService.GetZoneWaterUsage(startTime, endTime)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, usage)
}
