// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package v1

import (
	"strings"

	"github.com/smallnewer/gogs/modules/auth/apiv1"
	"github.com/smallnewer/gogs/modules/base"
	"github.com/smallnewer/gogs/modules/middleware"
	"github.com/smallnewer/gogs/modules/setting"
)

// Render an arbitrary Markdown document.
func Markdown(ctx *middleware.Context, form apiv1.MarkdownForm) {
	if ctx.HasApiError() {
		ctx.APIError(422, "", ctx.GetErrMsg())
		return
	}

	if len(form.Text) == 0 {
		ctx.Write([]byte(""))
		return
	}

	switch form.Mode {
	case "gfm":
		ctx.Write(base.RenderMarkdown([]byte(form.Text),
			setting.AppUrl+strings.TrimPrefix(form.Context, "/")))
	default:
		ctx.Write(base.RenderRawMarkdown([]byte(form.Text), ""))
	}
}

// Render a Markdown document in raw mode.
func MarkdownRaw(ctx *middleware.Context) {
	body, err := ctx.Req.Body().Bytes()
	if err != nil {
		ctx.APIError(422, "", err)
		return
	}
	ctx.Write(base.RenderRawMarkdown(body, ""))
}
