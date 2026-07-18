# Network layout (DESIGN_v2 §6.2):
#   - Public subnets, two tiers: ALB (control API) and GameServer tasks
#     (ENI + public IP, clients connect directly — no game-traffic LB).
#   - Private subnets: arena-api / arena-controller / Redis.
#   - VPC endpoints so the control plane needs no NAT gateway.

data "aws_region" "current" {}

resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-vpc" })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-igw" })
}

locals {
  az_count = length(var.availability_zones)
}

# --- Subnets ---------------------------------------------------------------

resource "aws_subnet" "alb" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  availability_zone = var.availability_zones[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, count.index)

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb-${count.index}", Tier = "public-alb" })
}

resource "aws_subnet" "gameserver" {
  count = local.az_count

  vpc_id                  = aws_vpc.this.id
  availability_zone       = var.availability_zones[count.index]
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, 10 + count.index)
  map_public_ip_on_launch = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-gameserver-${count.index}", Tier = "public-gameserver" })
}

resource "aws_subnet" "private" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  availability_zone = var.availability_zones[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, 20 + count.index)

  tags = merge(var.tags, { Name = "${var.name_prefix}-private-${count.index}", Tier = "private" })
}

# --- Routing ---------------------------------------------------------------

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-public" })
}

resource "aws_route_table_association" "alb" {
  count = local.az_count

  subnet_id      = aws_subnet.alb[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "gameserver" {
  count = local.az_count

  subnet_id      = aws_subnet.gameserver[count.index].id
  route_table_id = aws_route_table.public.id
}

# Private subnets have no default route: all AWS access is via VPC endpoints.
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id

  tags = merge(var.tags, { Name = "${var.name_prefix}-private" })
}

resource "aws_route_table_association" "private" {
  count = local.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# --- Security groups -------------------------------------------------------

resource "aws_security_group" "alb" {
  name_prefix = "${var.name_prefix}-alb-"
  vpc_id      = aws_vpc.this.id
  description = "Control-plane ALB"

  ingress {
    description = "HTTPS from allowed CIDRs"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = var.allowed_cidrs
  }

  ingress {
    description = "HTTP from allowed CIDRs (dev without ACM cert)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = var.allowed_cidrs
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-alb" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "api" {
  name_prefix = "${var.name_prefix}-api-"
  vpc_id      = aws_vpc.this.id
  description = "arena-api tasks"

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-api" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group_rule" "api_from_alb" {
  type                     = "ingress"
  security_group_id        = aws_security_group.api.id
  source_security_group_id = aws_security_group.alb.id
  from_port                = 8080
  to_port                  = 8080
  protocol                 = "tcp"
  description              = "Control API via ALB"
}

resource "aws_security_group_rule" "api_from_gameserver" {
  type                     = "ingress"
  security_group_id        = aws_security_group.api.id
  source_security_group_id = aws_security_group.gameserver.id
  from_port                = 8080
  to_port                  = 8080
  protocol                 = "tcp"
  description              = "SDK Gateway gRPC stream from sidecars"
}

resource "aws_security_group" "controller" {
  name_prefix = "${var.name_prefix}-controller-"
  vpc_id      = aws_vpc.this.id
  description = "arena-controller tasks (outbound only)"

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-controller" })

  lifecycle {
    create_before_destroy = true
  }
}

# GameServer tasks: game traffic in from anywhere on the configured range;
# egress restricted to the SDK Gateway and the VPC endpoints (Logs etc).
resource "aws_security_group" "gameserver" {
  name_prefix = "${var.name_prefix}-gameserver-"
  vpc_id      = aws_vpc.this.id
  description = "GameServer tasks (direct client traffic)"

  ingress {
    description = "Game traffic UDP"
    from_port   = var.gameserver_port_range.from
    to_port     = var.gameserver_port_range.to
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "Game traffic TCP"
    from_port   = var.gameserver_port_range.from
    to_port     = var.gameserver_port_range.to
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-gameserver" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group_rule" "gameserver_to_api" {
  type                     = "egress"
  security_group_id        = aws_security_group.gameserver.id
  source_security_group_id = aws_security_group.api.id
  from_port                = 8080
  to_port                  = 8080
  protocol                 = "tcp"
  description              = "SDK Gateway"
}

resource "aws_security_group_rule" "gameserver_to_vpce" {
  type                     = "egress"
  security_group_id        = aws_security_group.gameserver.id
  source_security_group_id = aws_security_group.vpc_endpoints.id
  from_port                = 443
  to_port                  = 443
  protocol                 = "tcp"
  description              = "CloudWatch Logs / ECR via VPC endpoints"
}

resource "aws_security_group" "redis" {
  name_prefix = "${var.name_prefix}-redis-"
  vpc_id      = aws_vpc.this.id
  description = "ElastiCache Redis (control plane only, never game servers)"

  tags = merge(var.tags, { Name = "${var.name_prefix}-redis" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group_rule" "redis_from_api" {
  type                     = "ingress"
  security_group_id        = aws_security_group.redis.id
  source_security_group_id = aws_security_group.api.id
  from_port                = 6379
  to_port                  = 6379
  protocol                 = "tcp"
}

resource "aws_security_group_rule" "redis_from_controller" {
  type                     = "ingress"
  security_group_id        = aws_security_group.redis.id
  source_security_group_id = aws_security_group.controller.id
  from_port                = 6379
  to_port                  = 6379
  protocol                 = "tcp"
}

# --- VPC endpoints ---------------------------------------------------------

resource "aws_security_group" "vpc_endpoints" {
  name_prefix = "${var.name_prefix}-vpce-"
  vpc_id      = aws_vpc.this.id
  description = "Interface VPC endpoints"

  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-vpce" })

  lifecycle {
    create_before_destroy = true
  }
}

# Gateway endpoints (free): DynamoDB is on the hot path; S3 backs ECR layers.
resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.private.id, aws_route_table.public.id]

  tags = merge(var.tags, { Name = "${var.name_prefix}-dynamodb" })
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.private.id, aws_route_table.public.id]

  tags = merge(var.tags, { Name = "${var.name_prefix}-s3" })
}

locals {
  interface_endpoints = [
    "ecr.api",
    "ecr.dkr",
    "logs",
    "ecs",
    "sqs",
  ]
}

resource "aws_vpc_endpoint" "interface" {
  for_each = toset(local.interface_endpoints)

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${data.aws_region.current.name}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.vpc_endpoints.id]
  private_dns_enabled = true

  tags = merge(var.tags, { Name = "${var.name_prefix}-${each.value}" })
}
