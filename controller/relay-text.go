package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"one-api/common"
	"one-api/model"
	"strings"
)

func relayTextHelper(c *gin.Context, relayMode int) *OpenAIErrorWithStatusCode {
	channelType := c.GetInt("channel")
	tokenId := c.GetInt("token_id")
	consumeQuota := c.GetBool("consume_quota")
	group := c.GetString("group")
	var textRequest GeneralOpenAIRequest
	if consumeQuota || channelType == common.ChannelTypeAzure || channelType == common.ChannelTypePaLM {
		err := common.UnmarshalBodyReusable(c, &textRequest)
		if err != nil {
			return errorWrapper(err, "bind_request_body_failed", http.StatusBadRequest)
		}
	}
	if relayMode == RelayModeModeration && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	baseURL := common.ChannelBaseURLs[channelType]
	requestURL := c.Request.URL.String()
	if channelType == common.ChannelTypeCustom {
		baseURL = c.GetString("base_url")
	} else if channelType == common.ChannelTypeOpenAI {
		if c.GetString("base_url") != "" {
			baseURL = c.GetString("base_url")
		}
	}
	fullRequestURL := fmt.Sprintf("%s%s", baseURL, requestURL)
	if channelType == common.ChannelTypeAzure {
		// https://learn.microsoft.com/en-us/azure/cognitive-services/openai/chatgpt-quickstart?pivots=rest-api&tabs=command-line#rest-api
		query := c.Request.URL.Query()
		apiVersion := query.Get("api-version")
		if apiVersion == "" {
			apiVersion = c.GetString("api_version")
		}
		requestURL := strings.Split(requestURL, "?")[0]
		requestURL = fmt.Sprintf("%s?api-version=%s", requestURL, apiVersion)
		baseURL = c.GetString("base_url")
		task := strings.TrimPrefix(requestURL, "/v1/")
		model_ := textRequest.Model
		model_ = strings.Replace(model_, ".", "", -1)
		// https://github.com/songquanpeng/one-api/issues/67
		model_ = strings.TrimSuffix(model_, "-0301")
		model_ = strings.TrimSuffix(model_, "-0314")
		model_ = strings.TrimSuffix(model_, "-0613")
		fullRequestURL = fmt.Sprintf("%s/openai/deployments/%s/%s", baseURL, model_, task)
	} else if channelType == common.ChannelTypePaLM {
		err := relayPaLM(textRequest, c)
		return err
	}
	var promptTokens int
	switch relayMode {
	case RelayModeChatCompletions:
		promptTokens = countTokenMessages(textRequest.Messages, textRequest.Model)
	case RelayModeCompletions:
		promptTokens = countTokenInput(textRequest.Prompt, textRequest.Model)
	case RelayModeModeration:
		promptTokens = countTokenInput(textRequest.Input, textRequest.Model)
	}
	preConsumedTokens := common.PreConsumedQuota
	if textRequest.MaxTokens != 0 {
		preConsumedTokens = promptTokens + textRequest.MaxTokens
	}
	modelRatio := common.GetModelRatio(textRequest.Model)
	groupRatio := common.GetGroupRatio(group)
	ratio := modelRatio * groupRatio
	preConsumedQuota := int(float64(preConsumedTokens) * ratio)
	if consumeQuota {
		err := model.PreConsumeTokenQuota(tokenId, preConsumedQuota)
		if err != nil {
			return errorWrapper(err, "pre_consume_token_quota_failed", http.StatusOK)
		}
	}
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, c.Request.Body)
	if err != nil {
		return errorWrapper(err, "new_request_failed", http.StatusOK)
	}
	if channelType == common.ChannelTypeAzure {
		key := c.Request.Header.Get("Authorization")
		key = strings.TrimPrefix(key, "Bearer ")
		req.Header.Set("api-key", key)
	} else {
		req.Header.Set("Authorization", c.Request.Header.Get("Authorization"))
	}
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	req.Header.Set("Connection", c.Request.Header.Get("Connection"))
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errorWrapper(err, "do_request_failed", http.StatusOK)
	}
	err = req.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_request_body_failed", http.StatusOK)
	}
	err = c.Request.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_request_body_failed", http.StatusOK)
	}
	var textResponse TextResponse
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	var streamResponseText string

	defer func() {
		if consumeQuota {
			quota := 0
			completionRatio := 1.34 // default for gpt-3
			if strings.HasPrefix(textRequest.Model, "gpt-4") {
				completionRatio = 2
			}
			if isStream {
				responseTokens := countTokenText(streamResponseText, textRequest.Model)
				quota = promptTokens + int(float64(responseTokens)*completionRatio)
			} else {
				quota = textResponse.Usage.PromptTokens + int(float64(textResponse.Usage.CompletionTokens)*completionRatio)
			}
			quota = int(float64(quota) * ratio)
			if ratio != 0 && quota <= 0 {
				quota = 1
			}
			quotaDelta := quota - preConsumedQuota
			err := model.PostConsumeTokenQuota(tokenId, quotaDelta)
			if err != nil {
				common.SysError("Error consuming token remain quota: " + err.Error())
			}
			tokenName := c.GetString("token_name")
			userId := c.GetInt("id")
			model.RecordLog(userId, model.LogTypeConsume, fmt.Sprintf("通过令牌「%s」使用模型 %s 消耗 %d 点额度（模型倍率 %.2f，分组倍率 %.2f）", tokenName, textRequest.Model, quota, modelRatio, groupRatio))
			model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
			channelId := c.GetInt("channel_id")
			model.UpdateChannelUsedQuota(channelId, quota)
		}
	}()

	if isStream {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}

			if i := strings.Index(string(data), "\n\n"); i >= 0 {
				return i + 2, data[0:i], nil
			}

			if atEOF {
				return len(data), data, nil
			}

			return 0, nil, nil
		})
		dataChan := make(chan string)
		stopChan := make(chan bool)
		go func() {
			for scanner.Scan() {
				data := scanner.Text()
				if len(data) < 6 { // must be something wrong!
					common.SysError("Invalid stream response: " + data)
					continue
				}
				dataChan <- data
				data = data[6:]
				if !strings.HasPrefix(data, "[DONE]") {
					switch relayMode {
					case RelayModeChatCompletions:
						var streamResponse ChatCompletionsStreamResponse
						err = json.Unmarshal([]byte(data), &streamResponse)
						if err != nil {
							common.SysError("Error unmarshalling stream response: " + err.Error())
							return
						}
						for _, choice := range streamResponse.Choices {
							streamResponseText += choice.Delta.Content
						}
					case RelayModeCompletions:
						var streamResponse CompletionsStreamResponse
						err = json.Unmarshal([]byte(data), &streamResponse)
						if err != nil {
							common.SysError("Error unmarshalling stream response: " + err.Error())
							return
						}
						for _, choice := range streamResponse.Choices {
							streamResponseText += choice.Text
						}
					}
				}
			}
			stopChan <- true
		}()
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("Transfer-Encoding", "chunked")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Stream(func(w io.Writer) bool {
			select {
			case data := <-dataChan:
				if strings.HasPrefix(data, "data: [DONE]") {
					data = data[:12]
				}
				c.Render(-1, common.CustomEvent{Data: data})
				return true
			case <-stopChan:
				return false
			}
		})
		err = resp.Body.Close()
		if err != nil {
			return errorWrapper(err, "close_response_body_failed", http.StatusOK)
		}
		return nil
	} else {
		if consumeQuota {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return errorWrapper(err, "read_response_body_failed", http.StatusOK)
			}
			err = resp.Body.Close()
			if err != nil {
				return errorWrapper(err, "close_response_body_failed", http.StatusOK)
			}
			err = json.Unmarshal(responseBody, &textResponse)
			if err != nil {
				return errorWrapper(err, "unmarshal_response_body_failed", http.StatusOK)
			}
			if textResponse.Error.Type != "" {
				return &OpenAIErrorWithStatusCode{
					OpenAIError: textResponse.Error,
					StatusCode:  resp.StatusCode,
				}
			}
			// Reset response body
			resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
		}
		// We shouldn't set the header before we parse the response body, because the parse part may fail.
		// And then we will have to send an error response, but in this case, the header has already been set.
		// So the client will be confused by the response.
		// For example, Postman will report error, and we cannot check the response at all.
		for k, v := range resp.Header {
			c.Writer.Header().Set(k, v[0])
		}
		c.Writer.WriteHeader(resp.StatusCode)
		_, err = io.Copy(c.Writer, resp.Body)
		if err != nil {
			return errorWrapper(err, "copy_response_body_failed", http.StatusOK)
		}
		err = resp.Body.Close()
		if err != nil {
			return errorWrapper(err, "close_response_body_failed", http.StatusOK)
		}
		return nil
	}
}
