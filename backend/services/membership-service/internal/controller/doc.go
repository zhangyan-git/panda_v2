// Package controller 是会员服务的 HTTP 边界。
//
// 它做三件事，且只做这三件：
//
//  1. 解析请求（query string 与 JSON body），解析不动就回 400；
//  2. 取调用者身份（requireAdmin / requireUser），取不到就回 401；
//  3. 把业务层的错误翻成 HTTP 状态码与**给用户看的中文**（writeMembershipError）。
//
// 它**不做业务判断**。任何「如果 X 就填 Y」的写法出现在这里，多半是业务规则走错了层——
// 那些规则要能在没有 HTTP 的情况下被测试。唯一的例外是分发的 switch：它是路径的形状，
// 不是业务的形状。
package controller
